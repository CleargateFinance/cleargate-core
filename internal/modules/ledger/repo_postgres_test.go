//go:build integration

package ledger_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/modules/ledger"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// ledgerFixture is a service wired to a throwaway database, plus two buckets
// to move value between.
type ledgerFixture struct {
	db      *fixtures.TestDB
	svc     *ledger.Service
	payer   id.LedgerAccountID
	payee   id.LedgerAccountID
	account id.AccountID
}

func newLedgerFixture(t *testing.T) *ledgerFixture {
	t.Helper()
	ctx := context.Background()

	db := fixtures.NewTestDB(t)
	svc := ledger.New(db.DB, nil).Service()

	payerAccount := id.AccountID("11111111-1111-1111-1111-111111111111")
	payeeAccount := id.AccountID("22222222-2222-2222-2222-222222222222")

	payer, err := svc.EnsureAccount(ctx, &payerAccount, ledger.TypeCustomerFunds, currency.USDC)
	require.NoError(t, err)

	payee, err := svc.EnsureAccount(ctx, &payeeAccount, ledger.TypePayeePayable, currency.USDC)
	require.NoError(t, err)

	return &ledgerFixture{db: db, svc: svc, payer: payer, payee: payee, account: payerAccount}
}

// fund puts starting value into the payer bucket, balanced against the
// platform's on-chain float, so the book still sums to zero.
func (f *ledgerFixture) fund(t *testing.T, amount string) {
	t.Helper()
	ctx := context.Background()

	float, err := f.svc.EnsureAccount(ctx, nil, ledger.TypeOnchainFloat, currency.USDC)
	require.NoError(t, err)

	_, err = f.svc.Post(ctx, ledger.Entry{
		Kind:  ledger.KindFunding,
		Asset: currency.USDC,
		Lines: ledger.Transfer(float, f.payer, usd(t, amount)),
	})
	require.NoError(t, err)
}

func TestEnsureAccount_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)

	again, err := f.svc.EnsureAccount(ctx, &f.account, ledger.TypeCustomerFunds, currency.USDC)
	require.NoError(t, err)
	assert.Equal(t, f.payer, again, "asking twice must return the same bucket, not create a second")
}

func TestEnsureAccount_SeparateBucketPerAssetAndRole(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)

	// A second asset is a different bucket, because two assets are never
	// fungible with each other.
	otherAsset, err := f.svc.EnsureAccount(ctx, &f.account, ledger.TypeCustomerFunds, currency.Asset("EURC"))
	require.NoError(t, err)
	assert.NotEqual(t, f.payer, otherAsset)

	// The same customer acting as a payee is also a different bucket, since
	// money owed to them is not money they can spend.
	asPayee, err := f.svc.EnsureAccount(ctx, &f.account, ledger.TypePayeePayable, currency.USDC)
	require.NoError(t, err)
	assert.NotEqual(t, f.payer, asPayee)
}

func TestPost_WritesEntryAndUpdatesBalances(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	f.fund(t, "10.00")

	entryID, err := f.svc.Post(ctx, ledger.Entry{
		Kind:  ledger.KindPayment,
		Asset: currency.USDC,
		Lines: ledger.Transfer(f.payer, f.payee, usd(t, "0.40")),
	})
	require.NoError(t, err)
	assert.NotEmpty(t, entryID)

	payerBalance, err := f.svc.Balance(ctx, f.payer, currency.USDC)
	require.NoError(t, err)
	assert.Equal(t, "9.6", payerBalance.String())

	payeeBalance, err := f.svc.Balance(ctx, f.payee, currency.USDC)
	require.NoError(t, err)
	assert.Equal(t, "0.4", payeeBalance.String())
}

func TestPost_RejectsUnbalancedEntryBeforeReachingTheDatabase(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)

	_, err := f.svc.Post(ctx, ledger.Entry{
		Kind:  ledger.KindPayment,
		Asset: currency.USDC,
		Lines: []ledger.Line{
			ledger.Debit(f.payer, usd(t, "0.40")),
			ledger.Credit(f.payee, usd(t, "0.30")),
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sum to zero")
}

// TestDatabaseRejectsUnbalancedEntry proves the invariant holds even when the
// application-level check is bypassed entirely, which is the case that matters
// for backfills, admin scripts and any future writer.
func TestDatabaseRejectsUnbalancedEntry(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)

	err := f.db.UoW.Do(ctx, func(ctx context.Context) error {
		var entryID string
		var createdAt time.Time

		row := f.db.QueryRow(ctx, `
			INSERT INTO journal_entry (kind, created_at)
			VALUES ('payment', now())
			RETURNING id, created_at`)
		if err := row.Scan(&entryID, &createdAt); err != nil {
			return err
		}

		// One line with no counter-line: the entry cannot balance.
		_, err := f.db.Exec(ctx, `
			INSERT INTO posting (journal_entry_id, ledger_account_id, amount, asset, created_at)
			VALUES ($1, $2, 5.00, 'USDC', $3)`,
			entryID, string(f.payer), createdAt)
		return err
	})

	require.Error(t, err, "the database must reject an unbalanced entry at commit")
	assert.Contains(t, err.Error(), "unbalanced")

	// The rejection must have taken the posting with it.
	balance, err := f.svc.Balance(ctx, f.payer, currency.USDC)
	require.NoError(t, err)
	assert.True(t, balance.IsZero(), "the rolled back posting must leave no trace")
}

// TestDatabaseRejectsMutation proves history cannot be edited or removed, so
// the book can always be replayed.
func TestDatabaseRejectsMutation(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	f.fund(t, "1.00")

	_, err := f.db.Exec(ctx, `UPDATE posting SET amount = 999 WHERE asset = 'USDC'`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "append-only")

	_, err = f.db.Exec(ctx, `DELETE FROM posting WHERE asset = 'USDC'`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "append-only")
}

// TestPost_RollsBackWithCallerTransaction is the property the payment flow
// depends on: a later failure must undo the money movement that preceded it.
func TestPost_RollsBackWithCallerTransaction(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	f.fund(t, "10.00")

	sentinel := errors.New("a later step failed")

	err := f.db.UoW.Do(ctx, func(ctx context.Context) error {
		if _, err := f.svc.Post(ctx, ledger.Entry{
			Kind:  ledger.KindPayment,
			Asset: currency.USDC,
			Lines: ledger.Transfer(f.payer, f.payee, usd(t, "2.50")),
		}); err != nil {
			return err
		}
		// Everything above succeeded, then something else in the same unit of
		// work fails.
		return sentinel
	})
	require.ErrorIs(t, err, sentinel)

	balance, err := f.svc.Balance(ctx, f.payer, currency.USDC)
	require.NoError(t, err)
	assert.Equal(t, "10", balance.String(), "the payment must have been undone entirely")
}

func TestRecordSpend_CreatesCountersOnFirstSpend(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	agent := id.AgentID("33333333-3333-3333-3333-333333333333")
	now := time.Now().UTC()

	totals, err := f.svc.RecordSpend(ctx, agent, usd(t, "0.40"), now)
	require.NoError(t, err)
	require.Len(t, totals, 2, "a spend counts against both the daily and monthly window")

	for _, total := range totals {
		assert.Equal(t, "0.4", total.Amount.String())
	}

	day, err := f.svc.SpentIn(ctx, agent, ledger.DayPeriod(now), currency.USDC)
	require.NoError(t, err)
	assert.Equal(t, "0.4", day.String())
}

func TestSpentIn_IsZeroWhenNothingSpentYet(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)

	spent, err := f.svc.SpentIn(ctx,
		id.AgentID("44444444-4444-4444-4444-444444444444"),
		ledger.DayPeriod(time.Now()), currency.USDC)
	require.NoError(t, err)
	assert.True(t, spent.IsZero(), "an agent that has never spent must read as zero, not error")
}

// TestRecordSpend_IsAtomicUnderConcurrency is the race the counter design
// exists to prevent. Reading a total and writing back total plus amount would
// lose increments here.
func TestRecordSpend_IsAtomicUnderConcurrency(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	agent := id.AgentID("55555555-5555-5555-5555-555555555555")
	now := time.Now().UTC()

	const concurrent = 50

	var wg sync.WaitGroup
	errs := make(chan error, concurrent)

	for i := 0; i < concurrent; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.svc.RecordSpend(ctx, agent, usd(t, "1"), now); err != nil {
				errs <- err
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	spent, err := f.svc.SpentIn(ctx, agent, ledger.DayPeriod(now), currency.USDC)
	require.NoError(t, err)
	assert.Equal(t, "50", spent.String(), "every concurrent increment must be counted exactly once")
}

func TestCheckBook_ReportsZeroForAConsistentBook(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	f.fund(t, "10.00")

	_, err := f.svc.Post(ctx, ledger.Entry{
		Kind:  ledger.KindPayment,
		Asset: currency.USDC,
		Lines: ledger.Transfer(f.payer, f.payee, usd(t, "3.25")),
	})
	require.NoError(t, err)

	check, err := f.svc.CheckBook(ctx, currency.USDC)
	require.NoError(t, err)
	assert.True(t, check.Balanced(), "every posting in the book must cancel out, got %s", check.Total)
}

func TestCheckSpendCounters_ReportsNoDriftWhenCountersMatchPostings(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	f.fund(t, "10.00")

	agent := id.AgentID("66666666-6666-6666-6666-666666666666")
	now := time.Now().UTC()

	_, err := f.svc.Post(ctx, ledger.Entry{
		Kind:       ledger.KindPayment,
		Asset:      currency.USDC,
		Lines:      ledger.Transfer(f.payer, f.payee, usd(t, "0.40")),
		AgentID:    &agent,
		OccurredAt: now,
	})
	require.NoError(t, err)

	_, err = f.svc.RecordSpend(ctx, agent, usd(t, "0.40"), now)
	require.NoError(t, err)

	drifts, err := f.svc.CheckSpendCounters(ctx, ledger.DayPeriod(now))
	require.NoError(t, err)
	assert.Empty(t, drifts, "a counter matching its postings must not be reported as drift")
}

// TestCheckSpendCounters_DetectsDrift is what makes the counters trustworthy:
// they are a cache, and a cache that is never verified is only a guess.
func TestCheckSpendCounters_DetectsDrift(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	f.fund(t, "10.00")

	agent := id.AgentID("77777777-7777-7777-7777-777777777777")
	now := time.Now().UTC()

	// A payment of 0.40 is recorded in the book.
	_, err := f.svc.Post(ctx, ledger.Entry{
		Kind:       ledger.KindPayment,
		Asset:      currency.USDC,
		Lines:      ledger.Transfer(f.payer, f.payee, usd(t, "0.40")),
		AgentID:    &agent,
		OccurredAt: now,
	})
	require.NoError(t, err)

	// The counter says something else, as it would after a lost update.
	_, err = f.svc.RecordSpend(ctx, agent, usd(t, "5.00"), now)
	require.NoError(t, err)

	drifts, err := f.svc.CheckSpendCounters(ctx, ledger.DayPeriod(now))
	require.NoError(t, err)
	require.Len(t, drifts, 1)

	assert.Equal(t, agent, drifts[0].AgentID)
	assert.Equal(t, "5", drifts[0].Counter.String())
	assert.Equal(t, "0.4", drifts[0].Recomputed.String())
}

func TestEnsurePartitions_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)

	require.NoError(t, f.svc.EnsurePartitions(ctx, 6))
	require.NoError(t, f.svc.EnsurePartitions(ctx, 6), "re-running must not fail on existing partitions")
}

// TestPost_AcrossPartitionBoundary checks that an entry dated in another month
// lands in its own partition and is still visible to ordinary queries.
func TestPost_AcrossPartitionBoundary(t *testing.T) {
	ctx := context.Background()
	f := newLedgerFixture(t)
	f.fund(t, "10.00")

	lastMonth := time.Now().UTC().AddDate(0, -1, 0)

	_, err := f.svc.Post(ctx, ledger.Entry{
		Kind:       ledger.KindPayment,
		Asset:      currency.USDC,
		Lines:      ledger.Transfer(f.payer, f.payee, usd(t, "1.00")),
		OccurredAt: lastMonth,
	})
	require.NoError(t, err)

	balance, err := f.svc.Balance(ctx, f.payer, currency.USDC)
	require.NoError(t, err)
	assert.Equal(t, "9", balance.String(), "a posting in an older partition must still count")

	check, err := f.svc.CheckBook(ctx, currency.USDC)
	require.NoError(t, err)
	assert.True(t, check.Balanced())
}
