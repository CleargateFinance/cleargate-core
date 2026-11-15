// Package server owns the Gin engine, the middleware chain and the single
// mapping from apperr.Kind to HTTP status. Modules receive a *gin.RouterGroup
// and never construct an engine themselves, so no module can install global
// middleware or change the ordering the whole application depends on.
package server

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/gin-gonic/gin"
)

// Options configures the engine. Plain data rather than a config.Server, so
// this package does not depend on the config package.
type Options struct {
	// Mode selects the Gin mode, either "debug" or "release".
	Mode string

	// TrustedProxies are the only peers whose X-Forwarded-For header
	// c.ClientIP believes. Nil trusts nobody. Gin's own default trusts every
	// peer, which lets a client choose the address its rate limits and login
	// throttle are counted against.
	TrustedProxies []string

	// ClientIPHeader, when set, names the header the edge puts the client's
	// address in, such as CloudFront's CloudFront-Viewer-Address. The address
	// is then read from it alone and X-Forwarded-For is never believed, so it
	// excludes TrustedProxies. It is safe only when nothing but the edge can
	// reach the server; see ClientAddressFrom.
	ClientIPHeader string
}

// New builds the Gin engine with the standard middleware chain already
// installed, in the order every request must pass through them.
//
// Order matters and is deliberate. With a client IP header configured,
// ClientAddressFrom runs before all of these, so the address is settled first.
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
func New(opts Options, log *slog.Logger) (*gin.Engine, error) {
	gin.SetMode(ginMode(opts.Mode))

	if opts.ClientIPHeader != "" && len(opts.TrustedProxies) > 0 {
		return nil, errors.New("server: a client IP header and trusted proxies are two answers to one question; set one")
	}

	engine := gin.New()
	if err := engine.SetTrustedProxies(opts.TrustedProxies); err != nil {
		return nil, fmt.Errorf("server: trusted proxies: %w", err)
	}

	// The client's address is settled before anything else runs, so every
	// later middleware and handler sees the same one.
	if opts.ClientIPHeader != "" {
		if opts.Mode == "debug" {
			log.Warn("trusting a header for the client's address with nothing in front of the API: any client can set it and choose its own rate limit",
				slog.String("header", opts.ClientIPHeader))
		}
		engine.Use(ClientAddressFrom(opts.ClientIPHeader))
	}

	engine.Use(
		RequestID(),
		Recovery(log),
		RequestLogger(log),
		ErrorMapper(),
	)

	return engine, nil
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
