package ledger

import (
	"context"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// BookCheck is the result of summing every posting in the book in one asset.
//
// Double-entry gives this check for free: if the total is anything but zero,
// money was created or destroyed by a defect rather than by a real movement.
// It is the cheapest possible proof that the book is internally consistent.
type BookCheck struct {
	Asset     currency.Asset
	Total     currency.Amount
	CheckedAt time.Time
}

// Balanced reports whether the book sums to zero, as it always must.
func (c BookCheck) Balanced() bool { return c.Total.IsZero() }

// CheckBook sums every posting in the book, one check per asset that has any
// postings, so an empty book returns none.
//
// Every asset is summed, not only USD, because the check is for defects, and a
// defect is exactly what would write a posting in an asset nothing expects.
// A non-zero result means an invariant the database itself enforces has
// somehow been violated, so it is treated as an emergency rather than a
// warning. Running it on a schedule catches such a break close to when it
// happened, instead of during an audit months later.
func (s *Service) CheckBook(ctx context.Context) ([]BookCheck, error) {
	totals, err := s.repo.BookTotals(ctx)
	if err != nil {
		return nil, err
	}

	now := s.clock.Now()
	checks := make([]BookCheck, 0, len(totals))
	for _, total := range totals {
		checks = append(checks, BookCheck{Asset: total.Asset(), Total: total, CheckedAt: now})
	}
	return checks, nil
}

// CheckSpendCounters compares every stored counter for a window against the
// value recomputed from the postings behind it, and returns the mismatches.
//
// The counters exist because summing postings on the authorization path would
// be too slow, so they are a maintained cache of a derivable figure. Anything
// cached can drift, which is why the derivation is re-run on a schedule and
// the two are compared.
func (s *Service) CheckSpendCounters(ctx context.Context, p Period) ([]Drift, error) {
	stored, recomputed, err := s.repo.SpendSnapshot(ctx, p)
	if err != nil {
		return nil, err
	}

	// Key both sides by agent and asset so they can be compared directly, and
	// so a counter present on one side but missing on the other is still
	// reported rather than skipped.
	type key struct {
		agent id.AgentID
		asset currency.Asset
	}

	storedBy := make(map[key]currency.Amount, len(stored))
	for _, sp := range stored {
		storedBy[key{sp.AgentID, sp.Asset}] = sp.Amount
	}

	recomputedBy := make(map[key]currency.Amount, len(recomputed))
	for _, sp := range recomputed {
		recomputedBy[key{sp.AgentID, sp.Asset}] = sp.Amount
	}

	seen := make(map[key]struct{}, len(storedBy)+len(recomputedBy))
	var drifts []Drift

	compare := func(k key) {
		if _, done := seen[k]; done {
			return
		}
		seen[k] = struct{}{}

		counter, hasCounter := storedBy[k]
		if !hasCounter {
			counter = currency.Zero(k.asset)
		}
		actual, hasActual := recomputedBy[k]
		if !hasActual {
			actual = currency.Zero(k.asset)
		}

		if counter.Equal(actual) {
			return
		}

		drifts = append(drifts, Drift{
			AgentID:    k.agent,
			Period:     p,
			Asset:      k.asset,
			Counter:    counter,
			Recomputed: actual,
		})
	}

	for k := range storedBy {
		compare(k)
	}
	for k := range recomputedBy {
		compare(k)
	}

	return drifts, nil
}
