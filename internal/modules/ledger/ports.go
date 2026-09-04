package ledger

import (
	"context"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Repository is the storage this module needs.
//
// It is declared here, by the consumer, rather than by whatever implements it.
// Service depends only on this interface, so the Postgres implementation can
// be swapped for a fake in tests, or for a remote one later, without service
// code changing.
type Repository interface {
	// EnsureAccount returns the bucket for this owner, type and asset,
	// creating it if it does not exist yet. accountID is nil for platform
	// owned buckets.
	EnsureAccount(ctx context.Context, accountID *id.AccountID, typ AccountType, asset currency.Asset) (id.LedgerAccountID, error)

	// InsertEntry writes an entry and its postings. Implementations join
	// whatever transaction is open on ctx, so a caller can bundle this with
	// other writes atomically.
	InsertEntry(ctx context.Context, e Entry) (id.JournalEntryID, error)

	// Balance sums every posting against one bucket.
	Balance(ctx context.Context, ledgerAccountID id.LedgerAccountID, asset currency.Asset) (currency.Amount, error)

	// BookTotal sums every posting in the book, which must always be zero.
	BookTotal(ctx context.Context, asset currency.Asset) (currency.Amount, error)

	// IncrementSpend adds amount to an agent's counter for one window and
	// returns the new total, in a single atomic statement.
	IncrementSpend(ctx context.Context, agentID id.AgentID, p Period, amount currency.Amount) (currency.Amount, error)

	// SpentIn reports an agent's counter for one window, zero when no counter
	// exists yet.
	SpentIn(ctx context.Context, agentID id.AgentID, p Period, asset currency.Asset) (currency.Amount, error)

	// StoredSpend lists every counter recorded for one window.
	StoredSpend(ctx context.Context, p Period) ([]Spend, error)

	// RecomputeSpend derives what every agent's counter should be for one
	// window, by summing the payment postings attributed to each agent.
	RecomputeSpend(ctx context.Context, p Period) ([]Spend, error)

	// EnsurePartitions creates any missing monthly partitions, covering
	// monthsAhead months from now.
	EnsurePartitions(ctx context.Context, monthsAhead int) error
}

// Clock supplies the current time.
//
// Injecting it keeps period boundaries and entry timestamps testable, since a
// test can place a payment in any window without waiting for real time.
type Clock interface {
	Now() time.Time
}
