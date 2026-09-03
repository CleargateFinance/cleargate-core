// Command api runs the Cleargate HTTP API, the hot path, under a 100ms
// decision budget.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
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
	// to stop. That cancellation is what starts the shutdown sequence below.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	api, cleanup, err := app.BuildAPI(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer cleanup()

	srv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      api.Engine,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
	}

	// The server runs on its own goroutine so this one can wait on the signal.
	// Any startup failure is sent back over a channel, since a goroutine has no
	// other way to report one.
	serverErr := make(chan error, 1)
	go func() {
		log.Info("api listening", slog.String("addr", cfg.Server.Addr))
		// ErrServerClosed is the expected result of a deliberate Shutdown, so
		// it is not treated as a failure.
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	// Wait for whichever comes first: the server failing on its own, or a
	// shutdown signal.
	select {
	case err := <-serverErr:
		return err

	case <-ctx.Done():
		log.Info("shutdown signal received, draining connections")

		// A fresh context is required here. ctx is already cancelled, which is
		// what woke this branch, so passing it to Shutdown would abort
		// instantly and cut off the very requests we are trying to drain.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
		defer cancel()

		// Shutdown stops accepting new connections, then waits for in-flight
		// requests to finish. This is what stops a deploy from cutting a
		// payment in half.
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}

		log.Info("shutdown complete")
		return nil
	}
}
