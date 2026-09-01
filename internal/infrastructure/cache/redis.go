// Package cache provides the hot-path cache client (backed by Redis).
//
// Invariant: everything in cache is a rebuildable projection of the database.
// Losing it must degrade latency, never correctness.
package cache

// TODO(scaffold): client construction, health check.
