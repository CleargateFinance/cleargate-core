package fixtures

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/cache"
)

// TestCache wraps a cache.Client pointed at a throwaway Redis container.
type TestCache struct {
	*cache.Client

	// Addr is the host:port of the container, for tests that need to
	// connect separately or configure an application against it.
	Addr string
}

// NewTestCache starts a real Redis and returns a connected client.
//
// As with NewTestDB, the container's lifetime is bound to the test through
// t.Cleanup, so nothing is left running after the suite finishes.
func NewTestCache(t *testing.T) *TestCache {
	t.Helper()
	ctx := context.Background()

	container, err := tcredis.Run(ctx, "redis:7-alpine")
	if err != nil {
		t.Fatalf("fixtures: start redis: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("fixtures: terminate redis: %v", err)
		}
	})

	// The endpoint is requested without a scheme because cache.New expects a
	// bare host:port, not a redis:// URL.
	addr, err := container.Endpoint(ctx, "")
	if err != nil {
		t.Fatalf("fixtures: redis endpoint: %v", err)
	}

	client, err := cache.New(ctx, cache.Options{
		Addr:           addr,
		ConnectTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("fixtures: connect redis: %v", err)
	}
	t.Cleanup(func() {
		if err := client.Close(); err != nil {
			t.Logf("fixtures: close redis client: %v", err)
		}
	})

	return &TestCache{Client: client, Addr: addr}
}
