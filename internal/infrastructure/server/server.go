// Package server owns the Gin engine, the middleware chain and the single
// mapping from apperr.Kind to HTTP status. Modules receive a *gin.RouterGroup
// and never construct an engine themselves, so no module can install global
// middleware or change the ordering the whole application depends on.
package server

import (
	"log/slog"

	"github.com/gin-gonic/gin"
)

// Options configures the engine. Plain data rather than a config.Server, so
// this package does not depend on the config package.
type Options struct {
	// Mode selects the Gin mode, either "debug" or "release".
	Mode string
}

// New builds the Gin engine with the standard middleware chain already
// installed, in the order every request must pass through them.
//
// Order matters and is deliberate:
//
//  1. RequestID first, so every later middleware and handler can log with it.
//  2. Recovery second, so it can catch panics from everything after it.
//  3. RequestLogger third, so it observes the final status, including the 500
//     that Recovery writes when something panics.
//  4. ErrorMapper last, so it runs closest to the handlers whose errors it
//     translates.
//
// gin.New is used rather than gin.Default because Default installs Gin's own
// logger and recovery, which would duplicate ours and log in a different
// format.
func New(opts Options, log *slog.Logger) *gin.Engine {
	gin.SetMode(ginMode(opts.Mode))

	engine := gin.New()
	engine.Use(
		RequestID(),
		Recovery(log),
		RequestLogger(log),
		ErrorMapper(),
	)

	return engine
}

// ginMode translates our mode name to Gin's constant, defaulting to release.
//
// Release is the safe default because debug mode prints verbose output and
// warnings that do not belong in production. An unrecognised value should fail
// closed, toward the stricter setting.
func ginMode(mode string) string {
	if mode == "debug" {
		return gin.DebugMode
	}
	return gin.ReleaseMode
}
