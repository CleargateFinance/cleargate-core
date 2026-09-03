// Package logger configures structured logging.
//
// Every log line carries the request's trace ID so one payment can be followed
// across gateway, authorization, ledger and settlement.
package logger

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// ctxKey is the private tag under which a request-scoped logger is stored in
// a context. It is unexported so no other package can collide with it.
type ctxKey struct{}

// Options configures the logger. It is deliberately plain data rather than a
// config.Log, so this package does not depend on the config package.
type Options struct {
	// Level is one of debug, info, warn or error.
	Level string
	// Format is either "json" for production or "text" for local development.
	Format string
}

// New builds the application logger.
//
// JSON in production because log aggregators parse it, text locally because
// humans read it.
func New(opts Options) *slog.Logger {
	handlerOpts := &slog.HandlerOptions{Level: parseLevel(opts.Level)}

	var handler slog.Handler
	if strings.EqualFold(opts.Format, "json") {
		handler = slog.NewJSONHandler(os.Stdout, handlerOpts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, handlerOpts)
	}

	return slog.New(handler)
}

// WithContext returns a copy of ctx carrying log.
//
// Middleware puts a logger already tagged with the request ID here, so any
// code deeper in the call chain can log with that tag attached automatically,
// without every function needing a logger parameter.
func WithContext(ctx context.Context, log *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, log)
}

// FromContext returns the logger stored in ctx, or the default logger when
// nothing was stored. It never returns nil, so callers do not need a nil check.
func FromContext(ctx context.Context) *slog.Logger {
	if log, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return log
	}
	return slog.Default()
}

// parseLevel maps a level name to a slog level, defaulting to info on anything
// unrecognised rather than failing, since a bad log level should not stop the
// service from starting.
func parseLevel(name string) slog.Level {
	switch strings.ToLower(name) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
