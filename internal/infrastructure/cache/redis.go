// Package cache provides the hot-path cache client (backed by Redis).
//
// Invariant: everything in cache is a rebuildable projection of the database.
// Losing it must degrade latency, never correctness. Nothing authoritative is
// stored here, so a cold or wiped cache can never cause a wrong answer.
package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Options configures the client. Plain data rather than a config.Cache, so
// this package does not depend on the config package.
type Options struct {
	Addr           string
	Password       string
	DB             int
	ConnectTimeout time.Duration
}

// Client wraps the Redis client the rest of the application uses.
type Client struct {
	rdb *redis.Client
}

// New connects to Redis and verifies the connection is usable.
//
// As with the database, connecting eagerly and pinging means a bad address
// fails at boot rather than at the first cache read.
func New(ctx context.Context, opts Options) (*Client, error) {
	rdb := redis.NewClient(&redis.Options{
		Addr:     opts.Addr,
		Password: opts.Password,
		DB:       opts.DB,
	})

	pingCtx, cancel := context.WithTimeout(ctx, opts.ConnectTimeout)
	defer cancel()

	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("cache: ping: %w", err)
	}

	return &Client{rdb: rdb}, nil
}

// Redis exposes the underlying client for packages that need it directly.
func (c *Client) Redis() *redis.Client { return c.rdb }

// Ping reports whether the cache is reachable. It backs the health endpoint.
func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Close releases the connection. Call it once, at shutdown.
func (c *Client) Close() error {
	return c.rdb.Close()
}
