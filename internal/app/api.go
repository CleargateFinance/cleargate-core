package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/cache"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/config"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/server"
)

// API is the fully wired HTTP application, plus the resources it owns.
type API struct {
	// Engine is the HTTP handler, exposed so tests can drive it directly with
	// httptest rather than binding a real port.
	Engine *gin.Engine

	db    *database.DB
	cache *cache.Client
	log   *slog.Logger
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

	api := &API{
		Engine: server.New(server.Options{Mode: cfg.Server.Mode}, log),
		db:     db,
		cache:  cacheClient,
		log:    log,
	}

	api.registerRoutes()

	cleanup := func() {
		if err := cacheClient.Close(); err != nil {
			log.Error("closing cache", slog.String("error", err.Error()))
		}
		db.Close()
	}

	return api, cleanup, nil
}

// registerRoutes wires every module's routes onto the engine.
//
// Route grouping is a security boundary, not decoration. Two caller classes
// exist and must not overlap:
//
//	agentAPI   - API-key credential held by the SDK, may REQUEST payments
//	consoleAPI - human session, may CREATE and SIGN mandates
//
// Mandate-mutating routes will be registered only under consoleAPI, so an
// agent credential cannot reach them even if a handler forgets its check. The
// route simply does not exist on that surface.
//
// At this phase only the health endpoint exists. The groups arrive with the
// modules that need them, in Phase 6.
func (a *API) registerRoutes() {
	v1 := a.Engine.Group("/v1")
	v1.GET("/health", a.health)
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
