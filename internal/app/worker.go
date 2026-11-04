package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/config"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
	"github.com/CleargateFinance/cleargate-core/internal/modules/decisionlog"
	"github.com/CleargateFinance/cleargate-core/internal/modules/ledger"
	"github.com/CleargateFinance/cleargate-core/internal/modules/payment"
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
	decisionSvc := decisionlog.New(db).Service()
	keyRetention := payment.NewRetention(db, nil)

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
				every: 24 * time.Hour,
				run:   func(ctx context.Context) error { return checkSpendCounters(ctx, log, ledgerSvc) },
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
		// This should be unreachable, since the database rejects an unbalanced
		// entry. Reaching it means an invariant that cannot normally be
		// violated has been, so it is logged at error rather than warning and
		// is worth waking someone for.
		log.Error("ledger does not balance",
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
// it, for the windows currently open.
func checkSpendCounters(ctx context.Context, log *slog.Logger, svc *ledger.Service) error {
	now := time.Now().UTC()

	for _, p := range ledger.PeriodsFor(now) {
		drifts, err := svc.CheckSpendCounters(ctx, p)
		if err != nil {
			return err
		}

		for _, d := range drifts {
			log.Error("spend counter drift",
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
