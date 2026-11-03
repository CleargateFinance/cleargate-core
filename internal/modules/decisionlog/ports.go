package decisionlog

import (
	"context"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Repository is what this module needs from storage.
type Repository interface {
	// Insert stores a decision and its checks, joining the transaction open on
	// ctx. The caller's transaction is what makes the decision commit with
	// the payment it judged, or not at all.
	Insert(ctx context.Context, r Record) (id.DecisionID, error)

	// EnsurePartitions creates any missing monthly partitions of the decision
	// tables, covering monthsAhead months from now.
	EnsurePartitions(ctx context.Context, monthsAhead int) error
}
