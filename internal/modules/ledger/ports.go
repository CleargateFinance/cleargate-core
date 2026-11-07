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
	// PaymentEntry returns the entry that debited a payment, written at at.
	PaymentEntry(ctx context.Context, paymentID id.PaymentID, at time.Time) (id.JournalEntryID, error)

	// FundedTotal sums what funding entries credited these accounts, in asset.
	FundedTotal(ctx context.Context, accounts []id.AccountID, asset currency.Asset) (currency.Amount, error)

	// EnsureAccount returns the bucket for this owner, type and asset,
	// creating it if it does not exist yet. accountID is nil for platform
	// owned buckets.
	EnsureAccount(ctx context.Context, accountID *id.AccountID, typ AccountType, asset currency.Asset) (id.LedgerAccountID, error)

	// InsertEntry writes an entry and its postings. Implementations join
	// whatever transaction is open on ctx, so a caller can bundle this with
	// other writes atomically. Returns ErrInsufficientFunds, writing nothing,
	// if customer funds would go below zero.
	InsertEntry(ctx context.Context, e Entry) (id.JournalEntryID, error)

	// Balance sums every posting against one bucket.
	Balance(ctx context.Context, ledgerAccountID id.LedgerAccountID, asset currency.Asset) (currency.Amount, error)

	// BookTotals sums every posting in the book, one total per asset that has
	// any postings. Each must always be zero.
	BookTotals(ctx context.Context) ([]currency.Amount, error)

	// IncrementSpend adds amount to an agent's counter for one window and
	// returns the new total, in a single atomic statement.
	IncrementSpend(ctx context.Context, agentID id.AgentID, p Period, amount currency.Amount) (currency.Amount, error)

	// DecrementSpend takes amount off an agent's counter for one window,
	// refusing to take off more than the counter holds.
	DecrementSpend(ctx context.Context, agentID id.AgentID, p Period, amount currency.Amount) error

	// SpentIn reports an agent's counter for one window, zero when no counter
	// exists yet.
	SpentIn(ctx context.Context, agentID id.AgentID, p Period, asset currency.Asset) (currency.Amount, error)

	// SpendSnapshot reads every counter recorded for one window, and what
	// each should be, derived from the payment postings attributed to each
	// agent net of reversals of those payments.
	//
	// Both come from one statement, so from one snapshot. Read separately, a
	// payment committing between the two reads would be in one and not the
	// other, and would be reported as drift that does not exist.
	SpendSnapshot(ctx context.Context, p Period) (stored, recomputed []Spend, err error)

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
