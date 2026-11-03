package counterparty

import (
	"context"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Repository is the storage this module needs.
type Repository interface {
	// Resolve returns the counterparty with this identity, creating it,
	// first seen at at, when there is none. Two callers resolving a new
	// identity at once get the same counterparty.
	Resolve(ctx context.Context, identity Identity, at time.Time) (id.CounterpartyID, error)
}

// Clock supplies the current time, for when a counterparty was first seen.
type Clock interface {
	Now() time.Time
}
