package ledger

import (
	"context"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Service implements the ledger's use cases. It depends on the interfaces in
// ports.go, never on the storage implementation directly.
type Service struct {
	repo  Repository
	clock Clock
}

// systemClock is the default time source, used when none is injected.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// NewService builds a Service. Passing a nil clock selects the real one.
func NewService(repo Repository, clock Clock) *Service {
	if clock == nil {
		clock = systemClock{}
	}
	return &Service{repo: repo, clock: clock}
}

// EnsureAccount returns the bucket for this owner, type and asset, creating it
// on first use.
func (s *Service) EnsureAccount(
	ctx context.Context,
	accountID *id.AccountID,
	typ AccountType,
	asset currency.Asset,
) (id.LedgerAccountID, error) {
	if !typ.Valid() {
		return "", apperr.Invalid("ledger: unknown account type " + string(typ))
	}
	if asset == "" {
		return "", apperr.Invalid("ledger: account needs an asset")
	}
	return s.repo.EnsureAccount(ctx, accountID, typ, asset)
}

// Post records one economic event.
//
// The entry is validated here so a malformed one fails with a message naming
// the actual problem. The database enforces the balance rule again on commit,
// and that second check is the one that makes the invariant true, since it
// cannot be bypassed by any code path.
//
// This joins whatever transaction is already open on ctx, so a caller that
// needs the posting to land atomically alongside other writes gets that by
// wrapping the whole group in one unit of work.
func (s *Service) Post(ctx context.Context, e Entry) (id.JournalEntryID, error) {
	if e.OccurredAt.IsZero() {
		e.OccurredAt = s.clock.Now()
	}

	if err := e.Validate(); err != nil {
		return "", err
	}

	return s.repo.InsertEntry(ctx, e)
}

// Balance reports how much sits in one bucket.
//
// It is summed from the postings every time rather than kept as a stored
// number, so it cannot drift away from the movements that produced it.
func (s *Service) Balance(
	ctx context.Context,
	ledgerAccountID id.LedgerAccountID,
	asset currency.Asset,
) (currency.Amount, error) {
	if ledgerAccountID == "" {
		return currency.Amount{}, apperr.Invalid("ledger: balance needs a ledger account")
	}
	return s.repo.Balance(ctx, ledgerAccountID, asset)
}

// AccountBalance reports a customer's spendable balance, which is the sum of
// their customer funds bucket for that asset.
func (s *Service) AccountBalance(
	ctx context.Context,
	accountID id.AccountID,
	asset currency.Asset,
) (currency.Amount, error) {
	bucket, err := s.repo.EnsureAccount(ctx, &accountID, TypeCustomerFunds, asset)
	if err != nil {
		return currency.Amount{}, err
	}
	return s.repo.Balance(ctx, bucket, asset)
}

// RecordSpend adds an amount to every counter window a payment falls into, and
// returns the new totals.
//
// The increment is atomic per window, so two concurrent payments cannot both
// read the same stale total and both slip under a cap that only one of them
// fits beneath. The caller checks the returned totals against the mandate and
// rolls the surrounding transaction back if a cap was breached, which is safe
// precisely because the increment already happened inside that transaction.
func (s *Service) RecordSpend(
	ctx context.Context,
	agentID id.AgentID,
	amount currency.Amount,
	at time.Time,
) ([]Spend, error) {
	if agentID == "" {
		return nil, apperr.Invalid("ledger: spend needs an agent")
	}
	if amount.IsNegative() {
		return nil, apperr.Invalid("ledger: spend amount must not be negative")
	}
	if at.IsZero() {
		at = s.clock.Now()
	}

	periods := PeriodsFor(at)
	totals := make([]Spend, 0, len(periods))

	for _, p := range periods {
		total, err := s.repo.IncrementSpend(ctx, agentID, p, amount)
		if err != nil {
			return nil, err
		}
		totals = append(totals, Spend{
			AgentID: agentID,
			Period:  p,
			Asset:   amount.Asset(),
			Amount:  total,
		})
	}

	return totals, nil
}

// SpentIn reports how much an agent has spent in one window.
func (s *Service) SpentIn(
	ctx context.Context,
	agentID id.AgentID,
	p Period,
	asset currency.Asset,
) (currency.Amount, error) {
	if !p.Type.Valid() {
		return currency.Amount{}, apperr.Invalid("ledger: unknown period type " + string(p.Type))
	}
	return s.repo.SpentIn(ctx, agentID, p, asset)
}

// EnsurePartitions creates any monthly partitions that do not exist yet.
func (s *Service) EnsurePartitions(ctx context.Context, monthsAhead int) error {
	return s.repo.EnsurePartitions(ctx, monthsAhead)
}
