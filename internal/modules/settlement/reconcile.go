package settlement

import (
	"context"
	"errors"
	"fmt"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
)

// WalletCheck compares what the ledger promises one account with what its
// wallet holds on-chain.
type WalletCheck struct {
	Wallet Wallet

	// Funds is the account's customer funds in the ledger, and Held the
	// wallet's token converted at par, both in the unit of account.
	Funds, Held currency.Amount
}

// Short reports the ledger promising more than the wallet holds. Holding more
// is normal: a deposit lands before it is credited, a quarantined one is never
// credited, and a payment is debited before its transfer leaves the wallet.
func (c WalletCheck) Short() bool {
	short, err := c.Funds.GreaterThan(c.Held)
	// Both sides are USD by construction, so a mismatch is a defect, and a
	// check that cannot compare must not pass.
	return short || err != nil
}

// WalletReconciler ties the ledger to the chain (Task 7.7): every account's
// customer funds must be backed by the token in its wallet (§8.3). It is the
// one check that notices a wallet holding less than the ledger says, which the
// ledger's own zero-sum check cannot see, since the book balances either way.
//
// Like the deposit watcher it runs in the worker and needs no custodian.
type WalletReconciler struct {
	repo  Repository
	chain ChainReader
	funds FundsLedger
	token Token
}

// WalletReconcilerDeps are what a WalletReconciler needs. All are required: a
// check missing a side has nothing to compare.
type WalletReconcilerDeps struct {
	DB    *database.DB
	Chain ChainReader
	Funds FundsLedger
	Token Token
}

// NewWalletReconciler builds a WalletReconciler against Postgres.
func NewWalletReconciler(d WalletReconcilerDeps) (*WalletReconciler, error) {
	if d.DB == nil {
		return nil, fmt.Errorf("%w: database", ErrMissingDependency)
	}
	return newWalletReconciler(NewPostgresRepository(d.DB), d.Chain, d.Funds, d.Token)
}

func newWalletReconciler(repo Repository, chain ChainReader, funds FundsLedger, token Token) (*WalletReconciler, error) {
	switch {
	case chain == nil:
		return nil, fmt.Errorf("%w: chain", ErrMissingDependency)
	case funds == nil:
		return nil, fmt.Errorf("%w: ledger", ErrMissingDependency)
	case token.Network == "" || token.Decimals <= 0:
		return nil, fmt.Errorf("%w: token", ErrMissingDependency)
	}
	return &WalletReconciler{repo: repo, chain: chain, funds: funds, token: token}, nil
}

// CheckWallets compares every wallet's customer funds with its on-chain holding.
// One wallet that cannot be read does not stop the others: their checks are
// returned beside an error naming it, so an outage is never a clean pass.
//
// Each wallet is read ledger first, then chain, back to back. Every legitimate
// movement lowers the ledger before the chain (a payment is debited, then
// sent) or raises the chain before the ledger (a deposit is finalized, then
// credited), so a movement between the two reads can only make the wallet look
// fuller. Read the other way round, a deposit credited in between would look
// like a shortfall.
func (r *WalletReconciler) CheckWallets(ctx context.Context) ([]WalletCheck, error) {
	wallets, err := r.repo.Wallets(ctx, ChainSolana)
	if err != nil {
		return nil, err
	}
	var checks []WalletCheck
	var failures []error
	for _, w := range wallets {
		check, err := r.check(ctx, w)
		if err != nil {
			failures = append(failures, fmt.Errorf("wallet %s: %w", w.Address, err))
			continue
		}
		checks = append(checks, check)
	}
	return checks, errors.Join(failures...)
}

func (r *WalletReconciler) check(ctx context.Context, w Wallet) (WalletCheck, error) {
	funds, err := r.funds.CustomerFunds(ctx, w.AccountID)
	if err != nil {
		return WalletCheck{}, err
	}
	atomic, err := r.chain.TokenBalance(ctx, w.Address)
	if err != nil {
		return WalletCheck{}, err
	}
	held, err := r.token.ToUSD(atomic)
	if err != nil {
		return WalletCheck{}, err
	}
	return WalletCheck{Wallet: w, Funds: funds, Held: held}, nil
}
