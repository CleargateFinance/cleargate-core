//go:build integration

package app

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/config"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/solana"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/solana/solanatest"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// These tests run the wallet check through the worker's own wiring, after real
// deposits through the deposit watcher, against a real database and a fake
// node. What they check is the ledger and the chain agreeing, so neither side
// is faked below its wire format.

func newWalletCheck(t *testing.T, f *depositFixture) func() error {
	t.Helper()
	chain, err := solana.New(solana.Options{RPCURL: f.node.URL()})
	require.NoError(t, err)
	r, err := newWalletReconciler(f.db.DB, chain, config.Solana{
		Network: devnetRail.Network, USDCMint: solanatest.DevnetUSDC, USDCDecimals: 6,
	})
	require.NoError(t, err)
	return func() error { return checkWallets(context.Background(), slog.New(f.log), r) }
}

func shortfalls(log *alertLog) []map[string]string {
	var out []map[string]string
	for _, r := range log.alerts() {
		if r["alert"] == "funds_above_wallet" {
			out = append(out, r)
		}
	}
	return out
}

// TestChainReconciliation_FlagsFundsAboveWalletBalance: the wallet losing
// tokens the ledger still promises the account, as when a payment released as
// expired lands after all, pages with both figures.
func TestChainReconciliation_FlagsFundsAboveWalletBalance(t *testing.T) {
	f := newDepositFixture(t)
	check := newWalletCheck(t, f)
	f.deposit(10_000_000)
	f.poll()
	require.Equal(t, "10", f.balance())

	f.node.SetBalance(f.wallet, solanatest.DevnetUSDC, 4_000_000)
	require.NoError(t, check(), "finding a shortfall is the job succeeding")

	found := shortfalls(f.log)
	require.Len(t, found, 1)
	assert.Equal(t, string(f.accountID), found[0]["account_id"])
	assert.Equal(t, f.wallet, found[0]["address"])
	assert.Equal(t, "10", found[0]["funds"])
	assert.Equal(t, "4", found[0]["held"])
	assert.True(t, f.node.OnlyFinalized())
}

// TestChainReconciliation_QuietWhileTheWalletCoversTheFunds: a wallet nobody has
// paid yet, one holding exactly its credits, and one holding more than its
// credits (a deposit not yet seen, or quarantined) are all sound.
func TestChainReconciliation_QuietWhileTheWalletCoversTheFunds(t *testing.T) {
	f := newDepositFixture(t)
	check := newWalletCheck(t, f)

	require.NoError(t, check(), "no token account and no funds")

	f.deposit(2_500_000)
	f.poll()
	require.NoError(t, check(), "holding exactly what was credited")

	f.node.SetBalance(f.wallet, solanatest.DevnetUSDC, 3_000_000)
	require.NoError(t, check(), "holding more than was credited")

	assert.Empty(t, shortfalls(f.log))
}

// TestChainReconciliation_AnUnreadableChainFailsTheJob: a node outage is a
// failed check, never a clean one.
func TestChainReconciliation_AnUnreadableChainFailsTheJob(t *testing.T) {
	f := newDepositFixture(t)
	check := newWalletCheck(t, f)
	f.node.Fail("getTokenAccountsByOwner", true)

	err := check()

	require.Error(t, err)
	assert.Contains(t, err.Error(), f.wallet)
	assert.Empty(t, shortfalls(f.log))
}

// TestChainReconciliation_ChecksEveryWallet: the shortfall is reported for the
// account that has it, and only that one.
func TestChainReconciliation_ChecksEveryWallet(t *testing.T) {
	f := newDepositFixture(t)
	check := newWalletCheck(t, f)
	f.deposit(1_000_000)
	f.poll()

	other := newDepositFixtureOn(t, f)
	other.deposit(5_000_000)
	other.poll()
	f.node.SetBalance(other.wallet, solanatest.DevnetUSDC, 1_000_000)

	require.NoError(t, check())

	found := shortfalls(f.log)
	require.Len(t, found, 1)
	assert.Equal(t, string(other.accountID), found[0]["account_id"])
}

// newDepositFixtureOn adds a second principal, account and wallet beside f's,
// sharing its database, node, watcher and log.
func newDepositFixtureOn(t *testing.T, f *depositFixture) *depositFixture {
	t.Helper()
	ctx := context.Background()
	var principalID, accountID string
	require.NoError(t, f.db.QueryRow(ctx, `INSERT INTO principal (kind) VALUES ('person') RETURNING id`).Scan(&principalID))
	_, err := f.db.Exec(ctx, `INSERT INTO principal_identity (principal_id) VALUES ($1)`, principalID)
	require.NoError(t, err)
	require.NoError(t, f.db.QueryRow(ctx,
		`INSERT INTO account (principal_id, display_name) VALUES ($1, 'globex') RETURNING id`, principalID).Scan(&accountID))
	wallet := solanatest.RandomAddress(t)
	_, err = f.db.Exec(ctx,
		`INSERT INTO account_wallet (account_id, chain, provider, address) VALUES ($1, 'solana', 'cdp', $2)`, accountID, wallet)
	require.NoError(t, err)

	other := *f
	other.accountID, other.wallet = id.AccountID(accountID), wallet
	return &other
}
