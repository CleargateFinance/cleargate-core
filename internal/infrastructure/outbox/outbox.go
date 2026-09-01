// Package outbox implements the transactional outbox pattern.
//
// Domain events are written to the outbox table in the SAME transaction as the
// state change that produced them. A relay publishes them afterwards. This
// gives an exactly-once-effective event stream without a distributed
// transaction, and it is the seam through which the analytics warehouse and
// any future extracted service will be fed.
package outbox

// TODO(scaffold): Append(ctx, event) within tx; Relay worker.
