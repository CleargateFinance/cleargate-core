package settlement

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// steps records the order the reconciler reads in, across the ledger and the
// chain, since that order is what keeps a payment in flight from looking like
// a shortfall.
type steps []string

type stubFunds struct {
	steps *steps
	funds map[id.AccountID]string
	err   error
}

func (l stubFunds) CustomerFunds(_ context.Context, accountID id.AccountID) (currency.Amount, error) {
	*l.steps = append(*l.steps, "ledger "+string(accountID))
	if l.err != nil {
		return currency.Amount{}, l.err
	}
	return currency.Parse(l.funds[accountID], currency.USD)
}

type balanceChain struct {
	ChainReader // Anything a test does not fake panics rather than passes.

	steps    *steps
	balances map[string]int64
	failing  map[string]bool
}

func (c balanceChain) TokenBalance(_ context.Context, owner string) (*big.Int, error) {
	*c.steps = append(*c.steps, "chain "+owner)
	if c.failing[owner] {
		return nil, errors.New("node unavailable")
	}
	return big.NewInt(c.balances[owner]), nil
}

type walletsRepo struct {
	Repository
	wallets []Wallet
}

func (r walletsRepo) Wallets(context.Context, Chain) ([]Wallet, error) { return r.wallets, nil }

func newTestReconciler(t *testing.T, wallets []Wallet, funds stubFunds, chain balanceChain) *WalletReconciler {
	t.Helper()
	r, err := newWalletReconciler(walletsRepo{wallets: wallets}, chain, funds, testToken)
	require.NoError(t, err)
	return r
}

var (
	walletA = Wallet{AccountID: "acct-a", Chain: ChainSolana, Provider: "cdp", Address: addrA}
	walletB = Wallet{AccountID: "acct-b", Chain: ChainSolana, Provider: "cdp", Address: addrB}
)

// TestCheckWallets_FlagsFundsAboveTheWallet: the ledger promising an account
// more than its wallet holds is the shortfall; holding more than promised is
// normal, since a deposit is held before it is credited and a payment is
// debited before it leaves.
func TestCheckWallets_FlagsFundsAboveTheWallet(t *testing.T) {
	var s steps
	r := newTestReconciler(t, []Wallet{walletA, walletB},
		stubFunds{steps: &s, funds: map[id.AccountID]string{"acct-a": "10", "acct-b": "4"}},
		balanceChain{steps: &s, balances: map[string]int64{addrA: 9_999_999, addrB: 5_000_000}})

	checks, err := r.CheckWallets(context.Background())

	require.NoError(t, err)
	require.Len(t, checks, 2)
	assert.True(t, checks[0].Short(), "10 promised, 9.999999 held")
	assert.Equal(t, "10", checks[0].Funds.String())
	assert.Equal(t, "9.999999", checks[0].Held.String())
	assert.False(t, checks[1].Short(), "4 promised, 5 held")
}

func TestCheckWallets_EqualIsNotShort(t *testing.T) {
	var s steps
	r := newTestReconciler(t, []Wallet{walletA},
		stubFunds{steps: &s, funds: map[id.AccountID]string{"acct-a": "2.5"}},
		balanceChain{steps: &s, balances: map[string]int64{addrA: 2_500_000}})

	checks, err := r.CheckWallets(context.Background())

	require.NoError(t, err)
	require.Len(t, checks, 1)
	assert.False(t, checks[0].Short())
}

// TestWalletCheck_SidesThatCannotBeComparedAreShort: both sides are USD by
// construction, so anything else is a defect, and a check that cannot compare
// must page rather than pass.
func TestWalletCheck_SidesThatCannotBeComparedAreShort(t *testing.T) {
	funds, err := currency.Parse("1", currency.USD)
	require.NoError(t, err)
	held, err := currency.Parse("5", currency.Asset("USDC"))
	require.NoError(t, err)

	assert.True(t, WalletCheck{Wallet: walletA, Funds: funds, Held: held}.Short())
}

// TestCheckWallets_ReadsTheLedgerBeforeTheChain: every legitimate movement
// lowers the ledger before the chain (a payment is debited, then sent) or
// raises the chain before the ledger (a deposit lands, then is credited). Read
// in this order, a movement between the two reads can only make the wallet look
// fuller than promised. Read the other way, a deposit finalized between them
// would look like a shortfall.
func TestCheckWallets_ReadsTheLedgerBeforeTheChain(t *testing.T) {
	var s steps
	r := newTestReconciler(t, []Wallet{walletA, walletB},
		stubFunds{steps: &s, funds: map[id.AccountID]string{"acct-a": "0", "acct-b": "0"}},
		balanceChain{steps: &s, balances: map[string]int64{}})

	_, err := r.CheckWallets(context.Background())

	require.NoError(t, err)
	assert.Equal(t, steps{"ledger acct-a", "chain " + addrA, "ledger acct-b", "chain " + addrB}, s,
		"each wallet's two reads are back to back, ledger first")
}

// TestCheckWallets_AnUnreadableWalletIsAnErrorNotAPass: a node outage must not
// look like a clean check, and must not hide the other wallets' results.
func TestCheckWallets_AnUnreadableWalletIsAnErrorNotAPass(t *testing.T) {
	var s steps
	r := newTestReconciler(t, []Wallet{walletA, walletB},
		stubFunds{steps: &s, funds: map[id.AccountID]string{"acct-a": "1", "acct-b": "3"}},
		balanceChain{steps: &s, balances: map[string]int64{addrB: 1_000_000}, failing: map[string]bool{addrA: true}})

	checks, err := r.CheckWallets(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), addrA)
	require.Len(t, checks, 1, "the readable wallet is still checked")
	assert.Equal(t, walletB, checks[0].Wallet)
	assert.True(t, checks[0].Short())
}

func TestCheckWallets_ALedgerFailureIsAnError(t *testing.T) {
	var s steps
	r := newTestReconciler(t, []Wallet{walletA},
		stubFunds{steps: &s, err: errors.New("database down")},
		balanceChain{steps: &s})

	checks, err := r.CheckWallets(context.Background())

	require.Error(t, err)
	assert.Empty(t, checks)
	assert.Equal(t, steps{"ledger acct-a"}, s, "the chain is not read for a wallet whose funds are unknown")
}

func TestNewWalletReconciler_RefusesMissingDependencies(t *testing.T) {
	var s steps
	_, err := newWalletReconciler(walletsRepo{}, nil, stubFunds{steps: &s}, testToken)
	assert.ErrorIs(t, err, ErrMissingDependency)
	_, err = newWalletReconciler(walletsRepo{}, balanceChain{steps: &s}, nil, testToken)
	assert.ErrorIs(t, err, ErrMissingDependency)
	_, err = newWalletReconciler(walletsRepo{}, balanceChain{steps: &s}, stubFunds{steps: &s}, Token{})
	assert.ErrorIs(t, err, ErrMissingDependency)
}
