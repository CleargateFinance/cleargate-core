//go:build integration

// Package e2e holds end-to-end tests that exercise the fully wired
// application, rather than any single package in isolation.
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/app"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/config"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// defaultTimeout bounds connection attempts in tests. It is generous, since
// container startup on a cold CI runner is slower than on a warm laptop.
const defaultTimeout = 10 * time.Second

// TestHealth_ReturnsOKAgainstRealDependencies is the walking skeleton's proof.
//
// It builds the real application through the same BuildAPI that cmd/api uses,
// against a real Postgres and a real Redis, then makes a real HTTP request.
// Nothing here is mocked, which is the point. This is the test that catches
// "every package compiles individually but the pieces are not actually wired
// together".
func TestHealth_ReturnsOKAgainstRealDependencies(t *testing.T) {
	ctx := context.Background()

	db := fixtures.NewTestDB(t)
	redis := fixtures.NewTestCache(t)

	cfg := &config.Config{
		Server:   config.Server{Mode: "release"},
		Database: config.Database{DSN: db.DSN, MaxConns: 4, MinConns: 1},
		Cache:    config.Cache{Addr: redis.Addr},
		Log:      config.Log{Level: "error", Format: "text"},
	}
	// ConnectTimeout has no default here because this Config is built
	// directly rather than through config.Load, so it is set explicitly.
	cfg.Database.ConnectTimeout = defaultTimeout

	api, cleanup, err := app.BuildAPI(ctx, cfg, slog.Default())
	require.NoError(t, err, "the application must build against real dependencies")
	defer cleanup()

	// httptest.NewRecorder captures the response without binding a real port,
	// so the test cannot collide with anything already listening.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	api.Engine.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "health must report 200 when dependencies are reachable")

	body, err := io.ReadAll(rec.Body)
	require.NoError(t, err)

	var got struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	require.NoError(t, json.Unmarshal(body, &got), "health response must be valid JSON")

	assert.Equal(t, "ok", got.Status)
	assert.Equal(t, "ok", got.Checks["database"])
	assert.Equal(t, "ok", got.Checks["cache"])

	// The request ID middleware must tag every response, including this one,
	// since without it no request can be traced through the logs.
	assert.NotEmpty(t, rec.Header().Get("X-Request-ID"))
}
