package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/config"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/outbox"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/solana"
	"github.com/CleargateFinance/cleargate-core/internal/modules/account"
	"github.com/CleargateFinance/cleargate-core/internal/modules/decisionlog"
	"github.com/CleargateFinance/cleargate-core/internal/modules/ledger"
	"github.com/CleargateFinance/cleargate-core/internal/modules/payment"
	"github.com/CleargateFinance/cleargate-core/internal/modules/settlement"
)

// Worker is the background application, holding the jobs it runs and the
// resources they need.
//
// It is built from the same modules as the API but wired differently. Anything
// slow lives only here, which turns "nothing slow in the request path" from a
// code review convention into a fact about which binary the code runs in.
type Worker struct {
	jobs []job
	log  *slog.Logger
}

// job is one piece of recurring background work.
type job struct {
	name string
	// every is how long to wait between runs.
	every time.Duration
	run   func(context.Context) error
}

// BuildWorker constructs the background application.
//
// The returned cleanup function releases every resource opened here, in
// reverse order. Callers must call it, normally with defer.
func BuildWorker(ctx context.Context, cfg *config.Config, log *slog.Logger) (*Worker, func(), error) {
	db, err := database.New(ctx, database.Options{
		DSN:            cfg.Database.DSN,
		MaxConns:       cfg.Database.MaxConns,
		MinConns:       cfg.Database.MinConns,
		ConnectTimeout: cfg.Database.ConnectTimeout,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("app: database: %w", err)
	}

	ledgerSvc := ledger.New(db, nil).Service()
	decisionSvc := decisionlog.NewService(decisionlog.NewPostgresRepository(db))
	keyRetention := payment.NewRetention(db, nil)

	chain, err := solana.New(solana.Options{RPCURL: cfg.Solana.RPCURL})
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("app: %w", err)
	}
	deposits, err := newDepositWatcher(db, chain, cfg.Solana, nil, log)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("app: deposit watcher: %w", err)
	}
	settler, err := newSettler(db, chain, cfg.Solana, nil, log)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("app: settler: %w", err)
	}
	wallets, err := newWalletReconciler(db, chain, cfg.Solana)
	if err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("app: wallet reconciler: %w", err)
	}

	w := &Worker{
		log: log,
		jobs: []job{
			{
				// The book must sum to zero at all times. Anything else means
				// value was created or destroyed by a defect, so this runs
				// often enough to catch a break close to when it happened
				// rather than during an audit months later.
				name:  "ledger_book_check",
				every: time.Hour,
				run:   func(ctx context.Context) error { return checkBook(ctx, log, ledgerSvc) },
			},
			{
				// Spend counters are a maintained cache of a figure derivable
				// from the postings. Anything cached can drift, so the
				// derivation is re-run and the two compared.
				name:  "spend_counter_reconcile",
				every: counterCheckInterval,
				run: func(ctx context.Context) error {
					return checkSpendCounters(ctx, log, ledgerSvc, time.Now())
				},
			},
			{
				// Partitions are created ahead of time so a write never
				// arrives to find no partition waiting for its month.
				name:  "ledger_partition_maintenance",
				every: 24 * time.Hour,
				run: func(ctx context.Context) error {
					return ledgerSvc.EnsurePartitions(ctx, 3)
				},
			},
			{
				// The same for decisions, which are written on every request,
				// declines included. A missing partition would fail every
				// payment, not only the approved ones.
				name:  "decision_partition_maintenance",
				every: 24 * time.Hour,
				run: func(ctx context.Context) error {
					return decisionSvc.EnsurePartitions(ctx, 3)
				},
			},
			{
				// Idempotency keys are remembered for 24 hours (Task 5.2).
				// Authorize already ignores an older key, so this only stops
				// the table growing with every payment ever made.
				name:  "idempotency_key_expiry",
				every: time.Hour,
				run: func(ctx context.Context) error {
					n, err := keyRetention.ExpireKeys(ctx)
					if err == nil {
						log.Info("idempotency keys expired", slog.Int64("deleted", n))
					}
					return err
				},
			},
			{
				// Reads each wallet's finalized history and credits what
				// arrived. Often enough that a deposit shows within a minute,
				// rarely enough for a rate-limited node.
				name:  "deposit_detection",
				every: chainPollInterval,
				run:   deposits.Poll,
			},
			{
				// Moves authorized payments to settled, failed or expired, and
				// gives back the funds and budget of any that never went out.
				name:  "payment_settlement",
				every: chainPollInterval,
				run:   settler.Sweep,
			},
			{
				// The ledger balancing says nothing about whether the money is
				// there. This is the one check that the wallets hold what the
				// ledger promises their accounts (Task 7.7).
				name:  "wallet_reconcile",
				every: time.Hour,
				run:   func(ctx context.Context) error { return checkWallets(ctx, log, wallets) },
			},
		},
	}

	return w, db.Close, nil
}

// Run executes every job once at startup, then on its own interval, until ctx
// is cancelled.
//
// Running immediately rather than waiting for the first tick means a
// misconfigured or already broken environment is reported at deploy time
// instead of an hour later.
func (w *Worker) Run(ctx context.Context) error {
	for _, j := range w.jobs {
		go w.runJob(ctx, j)
	}

	<-ctx.Done()
	w.log.Info("worker shutting down")
	return nil
}

// runJob runs one job on its interval until ctx is cancelled.
//
// A failing job logs and waits for its next tick rather than stopping. These
// are monitoring jobs, so one transient database error must not silently end
// the checks for the lifetime of the process.
func (w *Worker) runJob(ctx context.Context, j job) {
	ticker := time.NewTicker(j.every)
	defer ticker.Stop()

	for {
		start := time.Now()
		if err := j.run(ctx); err != nil {
			w.log.Error("job failed",
				slog.String("job", j.name),
				slog.String("error", err.Error()),
			)
		} else {
			w.log.Info("job completed",
				slog.String("job", j.name),
				slog.Duration("took", time.Since(start)),
			)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// chainPollInterval is how often the worker reads the chain. A finalized block
// takes about 13 seconds to get there, so polling faster finds nothing new.
const chainPollInterval = 30 * time.Second

// newDepositWatcher wires the deposit path. It builds its own account service
// with only the funding gate's parts: the worker sends no mail and opens no
// secrets, so it holds neither a mailer nor a sealer.
func newDepositWatcher(
	db *database.DB, chain *solana.Client, c config.Solana, clock settlement.Clock, log *slog.Logger,
) (*settlement.DepositWatcher, error) {
	ledgerSvc := ledger.New(db, nil).Service()
	principals := account.NewPrincipals(db)
	gate := account.NewFundingGate(db, LedgerFundingHistory{Principals: principals, Ledger: ledgerSvc}, log)
	return settlement.NewDepositWatcher(settlement.DepositWatcherDeps{
		DB:     db,
		Chain:  SettlementChain{Solana: chain, Mint: c.USDCMint},
		Ledger: SettlementFundingLedger{Ledger: ledgerSvc},
		Gate:   SettlementFundingGate{Principals: principals, Accounts: gate},
		Token:  token(c),
		Clock:  clock,
		Log:    log,
	})
}

// newSettler wires the sweep. Like the deposit watcher it only reads the chain,
// so the worker still holds no credential that can move money.
func newSettler(
	db *database.DB, chain *solana.Client, c config.Solana, clock payment.Clock, log *slog.Logger,
) (*payment.Settler, error) {
	observer, err := settlement.NewTransferObserver(SettlementChain{Solana: chain, Mint: c.USDCMint})
	if err != nil {
		return nil, err
	}
	return payment.NewSettler(payment.SettlerDeps{
		DB:     db,
		Ledger: PaymentLedger{Ledger: ledger.New(db, nil).Service()},
		Chain:  PaymentOutcomes{Observer: observer},
		Outbox: outbox.NewWriter(db),
		Clock:  clock,
		Log:    log,
	})
}

// newWalletReconciler wires the chain check. It reads the ledger and the chain
// and writes neither.
func newWalletReconciler(db *database.DB, chain *solana.Client, c config.Solana) (*settlement.WalletReconciler, error) {
	return settlement.NewWalletReconciler(settlement.WalletReconcilerDeps{
		DB:    db,
		Chain: SettlementChain{Solana: chain, Mint: c.USDCMint},
		Funds: SettlementFundsLedger{Ledger: ledger.New(db, nil).Service()},
		Token: token(c),
	})
}

// checkWallets verifies that every wallet holds at least the customer funds
// the ledger records for its account.
//
// A wallet holding less means the ledger promises money that is not there: a
// payment released as expired that landed after all, a transfer nobody
// authorized, or a defect in crediting. Nothing else notices, because the
// book still sums to zero, so each one pages.
func checkWallets(ctx context.Context, log *slog.Logger, r *settlement.WalletReconciler) error {
	checks, err := r.CheckWallets(ctx)

	short := 0
	for _, c := range checks {
		if !c.Short() {
			continue
		}
		short++
		log.Error("customer funds above the wallet's balance",
			slog.String(alertKey, "funds_above_wallet"),
			slog.String("account_id", string(c.Wallet.AccountID)),
			slog.String("address", c.Wallet.Address),
			slog.String("funds", c.Funds.String()),
			slog.String("held", c.Held.String()),
		)
	}

	// An unreadable wallet fails the job, after the readable ones are
	// reported, so a node outage is never logged as a clean check.
	if err != nil {
		return err
	}
	if short == 0 {
		log.Info("wallets hold the customer funds", slog.Int("wallets", len(checks)))
	}
	return nil
}

// alertKey marks a log line that must reach a person, with a stable value
// naming what broke, so an alerting rule can match on it rather than on the
// wording of a message (§8.10). Nothing pages on it yet; that is Phase 8.
const alertKey = "alert"

// counterCheckInterval is how often spend counters are reconciled. A day, as
// Task 1.3 sets: the counters are cheap to trust between checks because the
// payment path writes them in the same transaction as the postings.
const counterCheckInterval = 24 * time.Hour

// checkBook verifies that every posting in the book cancels out, in every
// asset the book holds.
func checkBook(ctx context.Context, log *slog.Logger, svc *ledger.Service) error {
	checks, err := svc.CheckBook(ctx)
	if err != nil {
		return err
	}

	balanced := true
	for _, check := range checks {
		if check.Balanced() {
			continue
		}
		balanced = false
		// This should be unreachable, since the database rejects an
		// unbalanced entry. Reaching it means an invariant that cannot
		// normally be violated has been, so it is worth waking someone for.
		// The job itself succeeded: it found what it exists to find.
		log.Error("ledger does not balance",
			slog.String(alertKey, "ledger_imbalance"),
			slog.String("asset", string(check.Asset)),
			slog.String("total", check.Total.String()),
		)
	}

	if balanced {
		log.Info("ledger balances", slog.Int("assets", len(checks)))
	}
	return nil
}

// checkSpendCounters compares each agent's counter against the postings behind
// it, for every window open now and every window open a check interval ago.
//
// The earlier windows are what make the check complete. A window is checked
// whole only after it closes, and a daily job running at 15:00 would
// otherwise never see the payments made between 15:00 and midnight in that
// day's window, nor the last day of a month in the month's.
func checkSpendCounters(ctx context.Context, log *slog.Logger, svc *ledger.Service, now time.Time) error {
	for _, p := range reconcilePeriods(now.UTC(), counterCheckInterval) {
		drifts, err := svc.CheckSpendCounters(ctx, p)
		if err != nil {
			return err
		}

		for _, d := range drifts {
			log.Error("spend counter drift",
				slog.String(alertKey, "spend_counter_drift"),
				slog.String("agent_id", string(d.AgentID)),
				slog.String("period_type", string(d.Period.Type)),
				slog.String("period_start", d.Period.Start.Format(time.DateOnly)),
				slog.String("asset", string(d.Asset)),
				slog.String("counter", d.Counter.String()),
				slog.String("recomputed", d.Recomputed.String()),
			)
		}

		if len(drifts) == 0 {
			log.Info("spend counters reconcile",
				slog.String("period_type", string(p.Type)),
				slog.String("period_start", p.Start.Format(time.DateOnly)),
			)
		}
	}

	return nil
}

// reconcilePeriods is every window that holds now or now minus every, each
// once. Run every interval, it checks each window at least once after it has
// closed.
func reconcilePeriods(now time.Time, every time.Duration) []ledger.Period {
	type key struct {
		typ   ledger.PeriodType
		start int64
	}
	var out []ledger.Period
	seen := map[key]bool{}
	for _, at := range []time.Time{now.Add(-every), now} {
		for _, p := range ledger.PeriodsFor(at) {
			k := key{p.Type, p.Start.UnixNano()}
			if !seen[k] {
				seen[k] = true
				out = append(out, p)
			}
		}
	}
	return out
}
