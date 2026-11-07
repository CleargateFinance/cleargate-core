//go:build integration

package app

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/config"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/solana"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/solana/solanatest"
	"github.com/CleargateFinance/cleargate-core/internal/modules/ledger"
	"github.com/CleargateFinance/cleargate-core/internal/modules/settlement"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// These tests run the deposit watcher through the worker's own wiring, against
// a real database and a fake node, because what they check spans the chain, the
// funding gate, the ledger and the deposit table.

type depositFixture struct {
	t       *testing.T
	db      *fixtures.TestDB
	node    *solanatest.Fake
	log     *alertLog
	watcher *settlement.DepositWatcher

	accountID id.AccountID
	wallet    string
}

func newDepositFixture(t *testing.T) *depositFixture {
	t.Helper()
	ctx := context.Background()
	db := fixtures.NewTestDB(t)
	node := solanatest.New(t)

	chain, err := solana.New(solana.Options{RPCURL: node.URL()})
	require.NoError(t, err)
	log := &alertLog{}
	watcher, err := newDepositWatcher(db.DB, chain, config.Solana{
		Network: devnetRail.Network, USDCMint: solanatest.DevnetUSDC, USDCDecimals: 6,
	}, nil, slog.New(log))
	require.NoError(t, err)

	var principalID, accountID string
	require.NoError(t, db.QueryRow(ctx, `INSERT INTO principal (kind) VALUES ('person') RETURNING id`).Scan(&principalID))
	_, err = db.Exec(ctx, `INSERT INTO principal_identity (principal_id) VALUES ($1)`, principalID)
	require.NoError(t, err)
	require.NoError(t, db.QueryRow(ctx,
		`INSERT INTO account (principal_id, display_name) VALUES ($1, 'acme') RETURNING id`, principalID).Scan(&accountID))
	wallet := solanatest.RandomAddress(t)
	_, err = db.Exec(ctx,
		`INSERT INTO account_wallet (account_id, chain, provider, address) VALUES ($1, 'solana', 'cdp', $2)`, accountID, wallet)
	require.NoError(t, err)

	return &depositFixture{
		t: t, db: db, node: node, log: log, watcher: watcher, accountID: id.AccountID(accountID), wallet: wallet,
	}
}

// deposit lands atomic units of USDC in the wallet.
func (f *depositFixture) deposit(atomic uint64) string {
	f.t.Helper()
	return f.node.Transfer(false,
		solanatest.Holdings{Owner: f.wallet, Mint: solanatest.DevnetUSDC, After: atomic},
		solanatest.Holdings{Owner: solanatest.RandomAddress(f.t), Mint: solanatest.DevnetUSDC, Before: atomic},
	)
}

func (f *depositFixture) balance() string {
	f.t.Helper()
	b, err := ledger.New(f.db.DB, nil).Service().AccountBalance(context.Background(), f.accountID, currency.USD)
	require.NoError(f.t, err)
	return b.String()
}

func (f *depositFixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	require.NoError(f.t, f.db.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func (f *depositFixture) poll() {
	f.t.Helper()
	require.NoError(f.t, f.watcher.Poll(context.Background()))
}

func TestDeposit_CreditsTheAccountOnce(t *testing.T) {
	f := newDepositFixture(t)
	f.deposit(12_500_000)

	f.poll()
	f.poll()

	assert.Equal(t, "12.5", f.balance())
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM deposit WHERE status = 'credited'`))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM journal_entry WHERE kind = 'funding'`))
	assert.Zero(t, f.count(`SELECT count(*) FROM (SELECT asset FROM posting GROUP BY asset HAVING SUM(amount) <> 0) AS unbalanced`),
		"the book still balances")
	assert.True(t, f.node.OnlyFinalized(), "nothing is credited on less than finalized")
}

// TestDeposit_ReadsOnlyWhatIsNew: the cursor is what keeps a poll to the new
// transactions, on a node that rate-limits.
func TestDeposit_ReadsOnlyWhatIsNew(t *testing.T) {
	f := newDepositFixture(t)
	f.deposit(1_000_000)
	f.poll()
	require.Equal(t, 1, f.node.Calls("getTransaction"))

	f.poll()
	assert.Equal(t, 1, f.node.Calls("getTransaction"), "nothing new, nothing fetched")

	f.deposit(2_000_000)
	f.poll()
	assert.Equal(t, 2, f.node.Calls("getTransaction"))
	assert.Equal(t, "3", f.balance())
}

// TestDeposit_LosingTheCursorCreditsNothingTwice: the cursor saves calls and
// decides nothing, as after a crash between a credit and saving it.
func TestDeposit_LosingTheCursorCreditsNothingTwice(t *testing.T) {
	f := newDepositFixture(t)
	f.deposit(1_000_000)
	f.poll()

	_, err := f.db.Exec(context.Background(), `DELETE FROM deposit_cursor`)
	require.NoError(t, err)
	f.poll()

	assert.Equal(t, "1", f.balance())
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM deposit`))
}

// TestDeposit_ConcurrentWorkersCreditOnce: two workers at once, as during a
// rolling deploy.
func TestDeposit_ConcurrentWorkersCreditOnce(t *testing.T) {
	f := newDepositFixture(t)
	f.deposit(5_000_000)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = f.watcher.Poll(context.Background())
		}()
	}
	wg.Wait()

	for _, err := range errs {
		assert.NoError(t, err)
	}
	assert.Equal(t, "5", f.balance())
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM journal_entry WHERE kind = 'funding'`))
}

// TestDeposit_IgnoresWhatIsNotADeposit: a failed transfer moved nothing, another
// token is not money here, and a payment out is not money in.
func TestDeposit_IgnoresWhatIsNotADeposit(t *testing.T) {
	f := newDepositFixture(t)
	otherMint := solanatest.RandomAddress(t)

	f.node.Transfer(true, solanatest.Holdings{Owner: f.wallet, Mint: solanatest.DevnetUSDC, After: 9_000_000})
	f.node.Transfer(false,
		solanatest.Holdings{Owner: f.wallet, Mint: otherMint, After: 9_000_000},
		solanatest.Holdings{Owner: f.wallet, Mint: solanatest.DevnetUSDC, Before: 1, After: 1})
	f.node.Transfer(false, solanatest.Holdings{Owner: f.wallet, Mint: solanatest.DevnetUSDC, Before: 9_000_000, After: 1})

	f.poll()

	assert.Equal(t, "0", f.balance())
	assert.Zero(t, f.count(`SELECT count(*) FROM deposit`))
	assert.Zero(t, f.count(`SELECT count(*) FROM journal_entry`))
}

// TestDeposit_PastTheCapIsQuarantined: the tier cap applies to deposits. The
// tokens are recorded and flagged, the balance does not count them, and a later
// deposit that fits is still credited.
func TestDeposit_PastTheCapIsQuarantined(t *testing.T) {
	f := newDepositFixture(t)
	f.deposit(150_000_000) // over the basic tier's 100
	f.deposit(40_000_000)

	f.poll()

	assert.Equal(t, "40", f.balance())
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM deposit WHERE status = 'quarantined' AND journal_entry_id IS NULL`))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM deposit WHERE status = 'credited'`))

	alerts := f.log.alerts()
	require.Len(t, alerts, 1)
	assert.Equal(t, "deposit_quarantined", alerts[0]["alert"])
}

// TestDeposit_CapCountsWhatWasAlreadyFunded: the cap is on the total to date.
func TestDeposit_CapCountsWhatWasAlreadyFunded(t *testing.T) {
	f := newDepositFixture(t)
	f.deposit(60_000_000)
	f.poll()
	f.deposit(60_000_000)
	f.poll()

	assert.Equal(t, "60", f.balance())
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM deposit WHERE status = 'quarantined'`))
}

// TestDeposit_ANodeFailureResumesWhereItStopped: a transaction the node cannot
// yet return stops the wallet at that point, not past it.
func TestDeposit_ANodeFailureResumesWhereItStopped(t *testing.T) {
	f := newDepositFixture(t)
	f.deposit(1_000_000)
	f.poll()
	missing := f.deposit(2_000_000)
	f.node.Forget(missing)

	assert.Error(t, f.watcher.Poll(context.Background()))
	assert.Equal(t, "1", f.balance())

	f.node.Restore()
	f.poll()
	assert.Equal(t, "3", f.balance())
}
