// Package cache provides the hot-path cache client (backed by Redis).
//
// Invariant: everything in cache is a rebuildable projection of the database.
// Losing it must degrade latency, never correctness. Nothing authoritative is
// stored here, so a cold or wiped cache can never cause a wrong answer.
package cache

import (
	"context"
	"errors"
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

// Get reads a key, reporting whether it was present.
//
// A missing key is not an error: absence is the ordinary case for a cache, and
// making callers distinguish redis.Nil from a real failure invites treating
// both the same way, which is how a cache miss turns into a request failure.
func (c *Client) Get(ctx context.Context, key string) ([]byte, bool, error) {
	value, err := c.rdb.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("cache: get %s: %w", key, err)
	}
	return value, true, nil
}

// Set writes a key with an expiry.
//
// The TTL is required rather than optional. Everything here is a derived copy
// of something in Postgres, so an entry that never expires is an entry that
// can disagree with the source of truth indefinitely.
func (c *Client) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("cache: set %s: ttl must be positive", key)
	}
	if err := c.rdb.Set(ctx, key, value, ttl).Err(); err != nil {
		return fmt.Errorf("cache: set %s: %w", key, err)
	}
	return nil
}

// Delete removes a key. Removing an absent key is not an error, so a caller
// invalidating something that was never cached needs no special case.
func (c *Client) Delete(ctx context.Context, key string) error {
	if err := c.rdb.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("cache: delete %s: %w", key, err)
	}
	return nil
}

// TTL reports the remaining lifetime of a key. It exists so that callers, and
// tests, can verify an entry cannot outlive what it describes.
func (c *Client) TTL(ctx context.Context, key string) (time.Duration, error) {
	ttl, err := c.rdb.TTL(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("cache: ttl %s: %w", key, err)
	}
	return ttl, nil
}

// Ping reports whether the cache is reachable. It backs the health endpoint.
func (c *Client) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Close releases the connection. Call it once, at shutdown.
func (c *Client) Close() error {
	return c.rdb.Close()
}
