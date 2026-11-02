package decisionlog

import "context"

// Repository is what this module needs from storage.
type Repository interface {
	// EnsurePartitions creates any missing monthly partitions of the decision
	// tables, covering monthsAhead months from now.
	EnsurePartitions(ctx context.Context, monthsAhead int) error
}
