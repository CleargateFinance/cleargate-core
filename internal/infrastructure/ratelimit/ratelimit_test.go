package ratelimit

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
)

// fakeCounter counts in memory. ttl is what TTL reports for any key.
type fakeCounter struct {
	counts map[string]int64
	ttls   map[string]time.Duration
	ttl    time.Duration
	err    error
}

func newFakeCounter() *fakeCounter {
	return &fakeCounter{counts: map[string]int64{}, ttls: map[string]time.Duration{}, ttl: 40 * time.Second}
}

func (f *fakeCounter) Increment(_ context.Context, key string, ttl time.Duration) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	if _, ok := f.ttls[key]; !ok {
		f.ttls[key] = ttl
	}
	f.counts[key]++
	return f.counts[key], nil
}

func (f *fakeCounter) TTL(context.Context, string) (time.Duration, error) { return f.ttl, nil }

func newLimiter(t *testing.T, c Counter, limit int) *Limiter {
	t.Helper()
	l, err := New(c, "test", limit, time.Minute, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	return l
}

func TestLimiter_AllowsUpToTheLimitThenReportsTheWait(t *testing.T) {
	c := newFakeCounter()
	l := newLimiter(t, c, 3)
	ctx := context.Background()

	for i := range 3 {
		assert.Zero(t, l.Allow(ctx, "a"), "request %d is within the limit", i+1)
	}
	assert.Equal(t, 40*time.Second, l.Allow(ctx, "a"), "the rest of the window")
	assert.Zero(t, l.Allow(ctx, "b"), "each caller has its own count")
	assert.Equal(t, time.Minute, c.ttls["ratelimit:test:a"], "the window is set on the counter")
}

func TestLimiter_FailsOpenWhenTheCounterIsDown(t *testing.T) {
	c := newFakeCounter()
	c.err = errors.New("redis: connection refused")
	l := newLimiter(t, c, 1)

	for range 5 {
		assert.Zero(t, l.Allow(context.Background(), "a"))
	}
}

func TestLimiter_ScopesDoNotShareCounts(t *testing.T) {
	c := newFakeCounter()
	agents, err := New(c, "agent", 1, time.Minute, nil)
	require.NoError(t, err)
	users, err := New(c, "console", 1, time.Minute, nil)
	require.NoError(t, err)

	assert.Zero(t, agents.Allow(context.Background(), "same-id"))
	assert.Zero(t, users.Allow(context.Background(), "same-id"))
}

func TestNew_RefusesAMisconfiguredLimit(t *testing.T) {
	c := newFakeCounter()
	for name, args := range map[string]struct {
		limit  int
		window time.Duration
	}{
		"zero limit":  {0, time.Minute},
		"zero window": {10, 0},
	} {
		_, err := New(c, "test", args.limit, args.window, nil)
		assert.Error(t, err, name)
	}
	_, err := New(nil, "test", 10, time.Minute, nil)
	assert.Error(t, err, "no counter")
}

func TestMiddleware_RefusesWith429AndRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	l := newLimiter(t, newFakeCounter(), 1)
	var recorded error
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		c.Next()
		if len(c.Errors) > 0 {
			recorded = c.Errors.Last().Err
		}
	})
	engine.GET("/", l.Middleware(func(*gin.Context) (string, bool) { return "a", true }),
		func(c *gin.Context) { c.Status(http.StatusNoContent) })

	serve := func() int {
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		return rec.Code
	}

	assert.Equal(t, http.StatusNoContent, serve())
	serve()
	require.Error(t, recorded)
	assert.Equal(t, apperr.KindRateLimited, apperr.KindOf(recorded))
	assert.Equal(t, "rate_limited", apperr.CodeOf(recorded))
	assert.Equal(t, 40*time.Second, apperr.RetryAfterOf(recorded))
}

func TestMiddleware_RefusesARequestWithNoCaller(t *testing.T) {
	gin.SetMode(gin.TestMode)
	l := newLimiter(t, newFakeCounter(), 100)
	reached := false
	engine := gin.New()
	engine.GET("/", l.Middleware(func(*gin.Context) (string, bool) { return "", false }),
		func(*gin.Context) { reached = true })

	engine.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	assert.False(t, reached, "a request the limiter cannot attribute does not get through")
}
