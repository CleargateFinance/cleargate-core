// Package redis provides the hot-path cache client.
//
// Invariant: everything in Redis is a rebuildable projection of Postgres.
// Losing Redis must degrade latency, never correctness.
package redis

// TODO(scaffold): client construction, health check.
