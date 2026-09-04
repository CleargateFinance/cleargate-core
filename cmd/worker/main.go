// Command worker runs background processing: ledger reconciliation and
// partition maintenance today, settlement batching and scoring later.
//
// It is a separate binary from the API so that slow work cannot share a
// process with the request path, and so the two can be scaled and deployed
// independently.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/CleargateFinance/cleargate-core/internal/app"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/config"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/logger"
)

func main() {
	// run returns an error rather than calling os.Exit itself, so that every
	// deferred cleanup actually runs. os.Exit skips defers, which would leak
	// database connections on any failure path.
	if err := run(); err != nil {
		slog.Error("fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := logger.New(logger.Options{
		Level:  cfg.Log.Level,
		Format: cfg.Log.Format,
	})
	slog.SetDefault(log)

	// notifyContext cancels ctx when the process receives an interrupt or
	// termination signal, which is how a container orchestrator asks a service
	// to stop. Jobs watch that cancellation and stop between runs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	worker, cleanup, err := app.BuildWorker(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer cleanup()

	log.Info("worker starting")
	return worker.Run(ctx)
}
