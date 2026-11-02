package fixtures

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/cache"
)

// TestCache wraps a cache.Client pointed at a throwaway Redis.
type TestCache struct {
	*cache.Client

	// Addr is the host:port of the container, for tests that need to
	// connect separately or configure an application against it.
	Addr string
}

// sharedRedis is the one container a test process starts, for the reason given
// at sharedPostgres. Redis has no cheap per-test database, so tests take turns
// instead: NewTestCache holds inUse until the test ends and flushes on the way
// in and out. A test that runs in parallel with another waits for its turn
// rather than seeing the other's keys.
var sharedRedis struct {
	once sync.Once
	err  error
	addr string

	inUse sync.Mutex
}

// NewTestCache returns a connected client to an empty Redis.
//
// The container lives until the test process exits, where testcontainers'
// reaper removes it.
func NewTestCache(t *testing.T) *TestCache {
	t.Helper()
	ctx := context.Background()

	sharedRedis.once.Do(func() {
		container, err := tcredis.Run(ctx, "redis:7-alpine")
		if err != nil {
			sharedRedis.err = fmt.Errorf("start: %w", err)
			return
		}
		// The endpoint is requested without a scheme because cache.New expects
		// a bare host:port, not a redis:// URL.
		sharedRedis.addr, sharedRedis.err = container.Endpoint(ctx, "")
	})
	if sharedRedis.err != nil {
		t.Fatalf("fixtures: shared redis: %v", sharedRedis.err)
	}

	sharedRedis.inUse.Lock()
	t.Cleanup(sharedRedis.inUse.Unlock)

	client, err := cache.New(ctx, cache.Options{
		Addr:           sharedRedis.addr,
		ConnectTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("fixtures: connect redis: %v", err)
	}
	flush := func() {
		if err := client.Redis().FlushAll(context.Background()).Err(); err != nil {
			t.Logf("fixtures: flush redis: %v", err)
		}
	}
	flush()
	// Runs before the unlock above, so the next test starts from empty even
	// if it forgets to flush.
	t.Cleanup(func() {
		flush()
		if err := client.Close(); err != nil {
			t.Logf("fixtures: close redis client: %v", err)
		}
	})

	return &TestCache{Client: client, Addr: sharedRedis.addr}
}
