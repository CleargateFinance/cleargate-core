// Package ratelimit caps how many requests one caller makes in a window, for
// each of the API's surfaces (Task 6.1): per client address on the public
// surface, per agent on the agent surface, per person on the console.
//
// It is load shedding, not authentication. The login throttle in the account
// module is the defence against guessing, keyed on (address, email) with
// backoff; this caps the volume one caller can send at all, whatever it sends.
package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
)

// Counter is the shared atomic counter the limit is kept in, so every API
// instance counts against the same number. cache.Client implements it.
type Counter interface {
	// Increment adds one and returns the new count, setting ttl only when the
	// key is created, so the window is fixed from the first request rather
	// than sliding forward with each one.
	Increment(ctx context.Context, key string, ttl time.Duration) (int64, error)
	TTL(ctx context.Context, key string) (time.Duration, error)
}

// ErrLimited is what a caller over its limit gets: 429, with Retry-After.
var ErrLimited = apperr.RateLimited("too many requests, slow down").WithCode("rate_limited")

// errNoKey is a wiring defect: the middleware ran before whatever names the
// caller, so it cannot tell whose request this is.
var errNoKey = apperr.Internal("ratelimit: no caller to count against", nil)

// Limiter allows a number of requests per caller per window.
//
// The window is fixed, not sliding: a caller can send up to twice the limit
// across a window boundary. That is the price of one INCR per request, and
// for shedding load it is the right one, since the limit is a ceiling on
// volume and not an exact quota anyone is billed against.
type Limiter struct {
	counter Counter
	scope   string
	limit   int64
	window  time.Duration
	log     *slog.Logger
}

// New builds a limiter allowing limit requests per window. scope keeps the
// surfaces' counters apart, so an agent id and a user id can never collide.
func New(counter Counter, scope string, limit int, window time.Duration, log *slog.Logger) (*Limiter, error) {
	if counter == nil {
		return nil, errors.New("ratelimit: counter is required")
	}
	if limit <= 0 || window <= 0 {
		// A zero limit would refuse everything and a zero window nothing; both
		// are configuration mistakes, not policies.
		return nil, fmt.Errorf("ratelimit: %s needs a positive limit and window, got %d per %s", scope, limit, window)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Limiter{counter: counter, scope: scope, limit: int64(limit), window: window, log: log}, nil
}

// Allow counts one request from key and reports how long the caller must wait
// if it is over the limit, or zero if it may proceed.
//
// A counter that cannot be reached allows the request. Failing closed would
// turn a Redis blip into an outage of every surface at once, including the
// console a person needs to freeze a misbehaving agent; the limit sheds load,
// and nothing about money depends on it.
func (l *Limiter) Allow(ctx context.Context, key string) time.Duration {
	k := "ratelimit:" + l.scope + ":" + key
	n, err := l.counter.Increment(ctx, k, l.window)
	if err != nil {
		l.log.Warn("rate limit counter unavailable, allowing the request",
			slog.String("scope", l.scope), slog.String("error", err.Error()))
		return 0
	}
	if n <= l.limit {
		return 0
	}

	// The key's remaining lifetime is the rest of the window. When it cannot
	// be read, the whole window is the honest upper bound.
	wait, err := l.counter.TTL(ctx, k)
	if err != nil || wait <= 0 {
		return l.window
	}
	return wait
}

// Middleware refuses requests over the limit. keyOf names the caller; it
// reports false when the request carries no caller to name, which is a wiring
// defect and refused rather than counted against some shared bucket.
func (l *Limiter) Middleware(keyOf func(*gin.Context) (string, bool)) gin.HandlerFunc {
	return func(c *gin.Context) {
		key, ok := keyOf(c)
		if !ok {
			_ = c.Error(errNoKey)
			c.Abort()
			return
		}
		if wait := l.Allow(c.Request.Context(), key); wait > 0 {
			_ = c.Error(ErrLimited.WithRetryAfter(wait))
			c.Abort()
			return
		}
		c.Next()
	}
}
