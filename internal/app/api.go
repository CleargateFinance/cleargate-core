package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/cache"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/cdp"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/config"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/server"
	"github.com/CleargateFinance/cleargate-core/internal/modules/account"
	"github.com/CleargateFinance/cleargate-core/internal/modules/decisionlog"
	"github.com/CleargateFinance/cleargate-core/internal/modules/ledger"
	"github.com/CleargateFinance/cleargate-core/internal/modules/mandate"
	"github.com/CleargateFinance/cleargate-core/internal/modules/payment"
	"github.com/CleargateFinance/cleargate-core/internal/modules/settlement"
	"github.com/CleargateFinance/cleargate-core/internal/modules/summary"
)

// API is the fully wired HTTP application, plus the resources it owns.
type API struct {
	// Engine is the HTTP handler, exposed so tests can drive it directly with
	// httptest rather than binding a real port.
	Engine *gin.Engine

	db    *database.DB
	cache *cache.Client
	log   *slog.Logger

	accounts  *account.Module
	mandates  *mandate.Module
	decisions *decisionlog.Module
	summaries *summary.Module
	wallets   *settlement.Module

	// surfaces are kept so a test can add a probe route to each and check the
	// group's middleware, not only the routes registered today.
	surfaces surfaces
}

// BuildAPI constructs the HTTP application: infrastructure, modules,
// middleware chains and route groups.
//
// This is the composition root, the only place that knows which concrete
// implementation satisfies which interface. It lives here rather than in
// main.go because cmd/api and cmd/worker need the same pieces wired
// differently, and because a test can call this to get a real application
// without starting a process.
//
// The returned cleanup function releases every resource opened here, in
// reverse order. Callers must call it, normally with defer.
func BuildAPI(ctx context.Context, cfg *config.Config, log *slog.Logger) (*API, func(), error) {
	// Infrastructure first, since everything else depends on it. Each package
	// takes its own Options type, and this function does the translation from
	// config, so no infrastructure package depends on the config package.
	db, err := database.New(ctx, database.Options{
		DSN:            cfg.Database.DSN,
		MaxConns:       cfg.Database.MaxConns,
		MinConns:       cfg.Database.MinConns,
		ConnectTimeout: cfg.Database.ConnectTimeout,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("app: database: %w", err)
	}

	cacheClient, err := cache.New(ctx, cache.Options{
		Addr:           cfg.Cache.Addr,
		Password:       cfg.Cache.Password,
		DB:             cfg.Cache.DB,
		ConnectTimeout: cfg.Database.ConnectTimeout,
	})
	if err != nil {
		// The database is already open at this point, so it must be closed
		// before returning, otherwise a failed boot leaks connections.
		db.Close()
		return nil, nil, fmt.Errorf("app: cache: %w", err)
	}

	// From here a failure must release both connections it already holds.
	fail := func(err error) (*API, func(), error) {
		_ = cacheClient.Close()
		db.Close()
		return nil, nil, err
	}

	mailer, err := newMailer(cfg, log)
	if err != nil {
		return fail(err)
	}
	keyring, err := crypto.NewKeyring(cfg.Crypto.MasterKeys, cfg.Crypto.ActiveKeyID)
	if err != nil {
		return fail(fmt.Errorf("app: keyring: %w", err))
	}

	accounts, err := account.New(account.Deps{
		DB: db, Cache: cacheClient, Log: log, Mailer: mailer, Sealer: keyring,
	})
	if err != nil {
		return fail(fmt.Errorf("app: account module: %w", err))
	}
	mandates, err := mandate.New(mandate.Deps{
		DB: db, Log: log, Accounts: MandateAccounts{Accounts: accounts.Service()},
	})
	if err != nil {
		return fail(fmt.Errorf("app: mandate module: %w", err))
	}
	decisions, err := decisionlog.New(decisionlog.Deps{
		DB:       db,
		Accounts: DecisionAccounts{Accounts: accounts.Service()},
		Payments: DecisionPayments{Payments: payment.NewLookup(db)},
	})
	if err != nil {
		return fail(fmt.Errorf("app: decision log module: %w", err))
	}
	summaries, err := summary.New(summary.Deps{
		Accounts: SummaryAccounts{Accounts: accounts.Service()},
		Ledger:   SummaryLedger{Ledger: ledger.New(db, nil).Service()},
		Mandates: SummaryMandates{Mandates: mandates.Service()},
		Payments: SummaryPayments{Payments: payment.NewLookup(db)},
	})
	if err != nil {
		return fail(fmt.Errorf("app: summary module: %w", err))
	}

	custodian, err := cdp.New(cdp.Options{
		APIKeyID: cfg.CDP.APIKeyID, APIKeySecret: cfg.CDP.APIKeySecret,
		WalletSecret: cfg.CDP.WalletSecret, BaseURL: cfg.CDP.BaseURL,
	})
	if err != nil {
		return fail(fmt.Errorf("app: %w", err))
	}
	wallets, err := settlement.New(settlement.Deps{
		DB:       db,
		Wallets:  SettlementWallets{CDP: custodian},
		Accounts: SettlementAccounts{Accounts: accounts.Service()},
		Token:    token(cfg.Solana),
	})
	if err != nil {
		return fail(fmt.Errorf("app: settlement module: %w", err))
	}

	limits, err := newLimiters(cfg.RateLimit, cacheClient, log)
	if err != nil {
		return fail(fmt.Errorf("app: rate limits: %w", err))
	}
	engine, err := server.New(server.Options{Mode: cfg.Server.Mode, TrustedProxies: cfg.Server.TrustedProxies}, log)
	if err != nil {
		return fail(fmt.Errorf("app: %w", err))
	}

	api := &API{
		Engine:    engine,
		db:        db,
		cache:     cacheClient,
		log:       log,
		accounts:  accounts,
		mandates:  mandates,
		decisions: decisions,
		summaries: summaries,
		wallets:   wallets,
	}

	api.registerRoutes(limits)

	cleanup := func() {
		if err := cacheClient.Close(); err != nil {
			log.Error("closing cache", slog.String("error", err.Error()))
		}
		db.Close()
	}

	return api, cleanup, nil
}

// token says which token is money on the configured network.
func token(c config.Solana) settlement.Token {
	return settlement.Token{Symbol: "USDC", Network: c.Network, Mint: c.USDCMint, Decimals: c.USDCDecimals}
}

// healthResponse is the body returned by the health endpoint.
type healthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// health reports whether the service can reach its dependencies.
//
// It deliberately checks the database and cache rather than just returning
// 200. "The process is running" is nearly worthless as a signal, since an API
// that cannot reach Postgres is not actually serving anyone. A load balancer
// needs to know the difference, so an unreachable dependency returns 503 and
// takes this instance out of rotation.
//
// Both checks always run, even after one fails, so the response names every
// broken dependency at once instead of only the first.
func (a *API) health(c *gin.Context) {
	ctx := c.Request.Context()
	checks := make(map[string]string, 2)
	healthy := true

	if err := a.db.Ping(ctx); err != nil {
		checks["database"] = "unreachable"
		healthy = false
		a.log.Error("health: database unreachable", slog.String("error", err.Error()))
	} else {
		checks["database"] = "ok"
	}

	if err := a.cache.Ping(ctx); err != nil {
		checks["cache"] = "unreachable"
		healthy = false
		a.log.Error("health: cache unreachable", slog.String("error", err.Error()))
	} else {
		checks["cache"] = "ok"
	}

	if !healthy {
		c.JSON(http.StatusServiceUnavailable, healthResponse{
			Status: "degraded",
			Checks: checks,
		})
		return
	}

	c.JSON(http.StatusOK, healthResponse{Status: "ok", Checks: checks})
}
