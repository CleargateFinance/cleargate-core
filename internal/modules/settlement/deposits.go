package settlement

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
)

// depositPageSize is how many signatures one call asks the node for.
const depositPageSize = 200

// DepositWatcher turns USDC arriving in account wallets into ledger credits
// (Phase 8). It runs in the worker, because RPC unreliability must never reach
// the API (§8.1), and it needs no custodian: reading the chain moves nothing.
type DepositWatcher struct {
	repo   Repository
	uow    database.UnitOfWork
	chain  ChainReader
	ledger FundingLedger
	gate   FundingGate
	token  Token
	clock  Clock
	log    *slog.Logger
}

// DepositWatcherDeps are what a DepositWatcher needs. All are required but
// Clock and Log: crediting money with any of them missing has no safe meaning.
type DepositWatcherDeps struct {
	DB     *database.DB
	Chain  ChainReader
	Ledger FundingLedger
	Gate   FundingGate
	Token  Token
	Clock  Clock
	Log    *slog.Logger
}

// NewDepositWatcher builds a DepositWatcher against Postgres.
func NewDepositWatcher(d DepositWatcherDeps) (*DepositWatcher, error) {
	switch {
	case d.DB == nil:
		return nil, fmt.Errorf("%w: database", ErrMissingDependency)
	case d.Chain == nil:
		return nil, fmt.Errorf("%w: chain", ErrMissingDependency)
	case d.Ledger == nil:
		return nil, fmt.Errorf("%w: ledger", ErrMissingDependency)
	case d.Gate == nil:
		return nil, fmt.Errorf("%w: funding gate", ErrMissingDependency)
	case d.Token.Network == "" || d.Token.Decimals <= 0:
		return nil, fmt.Errorf("%w: token", ErrMissingDependency)
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	clock := d.Clock
	if clock == nil {
		clock = systemClock{}
	}
	return &DepositWatcher{
		repo: NewPostgresRepository(d.DB), uow: d.DB.UoW, chain: d.Chain, ledger: d.Ledger,
		gate: d.Gate, token: d.Token, clock: clock, log: log,
	}, nil
}

// Poll reads every wallet's new history and credits what arrived. One wallet's
// failure does not stop the others; it is reported, and that wallet resumes
// from its cursor on the next poll.
func (w *DepositWatcher) Poll(ctx context.Context) error {
	wallets, err := w.repo.Wallets(ctx, ChainSolana)
	if err != nil {
		return err
	}
	var failures []error
	for _, wallet := range wallets {
		if err := w.pollWallet(ctx, wallet); err != nil {
			failures = append(failures, fmt.Errorf("wallet %s: %w", wallet.Address, err))
		}
	}
	return errors.Join(failures...)
}

// pollWallet handles the wallet's unread transactions oldest first, advancing
// the cursor after each, so a failure part-way resumes at the one that failed.
func (w *DepositWatcher) pollWallet(ctx context.Context, wallet Wallet) error {
	tokenAccount, err := w.chain.TokenAccount(wallet.Address)
	if err != nil {
		return err
	}
	cursor, err := w.repo.DepositCursor(ctx, wallet.Address)
	if err != nil {
		return err
	}
	unread, err := w.unread(ctx, tokenAccount, cursor)
	if err != nil {
		return err
	}
	for _, sig := range unread {
		if !sig.Failed {
			if err := w.process(ctx, wallet, sig.Signature); err != nil {
				return err
			}
		}
		if err := w.repo.SaveDepositCursor(ctx, wallet.Address, sig.Signature); err != nil {
			return err
		}
	}
	return nil
}

// unread pages back from the newest signature to the cursor, and returns them
// oldest first.
func (w *DepositWatcher) unread(ctx context.Context, tokenAccount, cursor string) ([]ChainSignature, error) {
	var all []ChainSignature
	before := ""
	for {
		page, err := w.chain.Signatures(ctx, tokenAccount, ChainPage{Before: before, Until: cursor, Limit: depositPageSize})
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < depositPageSize {
			slices.Reverse(all)
			return all, nil
		}
		before = page[len(page)-1].Signature
	}
}

// process credits one transaction, if it brought the wallet any of the token.
// A payment out, or a transaction touching only other tokens, is not a deposit.
func (w *DepositWatcher) process(ctx context.Context, wallet Wallet, signature string) error {
	tx, err := w.chain.Transaction(ctx, signature, wallet.Address)
	if err != nil {
		return err
	}
	if tx.Failed || tx.TokenChange.Sign() <= 0 {
		return nil
	}
	amount, err := w.token.ToUSD(tx.TokenChange)
	if err != nil {
		return err
	}
	return w.record(ctx, wallet, signature, amount)
}

// record writes the deposit and, unless the funding gate refuses it, its ledger
// credit, in one transaction. A deposit already recorded fails the insert and
// rolls the credit back with it, so a rescan or a second worker changes nothing.
func (w *DepositWatcher) record(ctx context.Context, wallet Wallet, signature string, amount currency.Amount) error {
	seen, err := w.repo.DepositSeen(ctx, w.token.Network, signature, wallet.Address)
	if err != nil || seen {
		return err
	}
	allowed, err := w.gate.FundingAllowed(ctx, wallet.AccountID, amount)
	if err != nil {
		return err
	}

	at := w.clock.Now().UTC()
	err = w.uow.Do(ctx, func(ctx context.Context) error {
		d := Deposit{
			Network: w.token.Network, Signature: signature, AccountID: wallet.AccountID,
			Address: wallet.Address, Amount: amount, Status: DepositQuarantined,
		}
		if allowed {
			entry, err := w.ledger.CreditFunding(ctx, wallet.AccountID, amount, at)
			if err != nil {
				return err
			}
			d.Status, d.JournalEntryID = DepositCredited, &entry
		}
		return w.repo.InsertDeposit(ctx, d, at)
	})
	if errors.Is(err, errDepositSeen) {
		return nil
	}
	if err != nil {
		return err
	}

	if !allowed {
		// The stable field is what an alert routes on: tokens are in a
		// customer's wallet that their balance does not show.
		w.log.Error("deposit quarantined, past the account's funding cap",
			slog.String("alert", "deposit_quarantined"),
			slog.String("account_id", string(wallet.AccountID)),
			slog.String("signature", signature),
			slog.String("amount", amount.String()),
		)
		return nil
	}
	w.log.Info("deposit credited",
		slog.String("account_id", string(wallet.AccountID)),
		slog.String("signature", signature),
		slog.String("amount", amount.String()),
	)
	return nil
}

// ToUSD converts atomic token units to the unit of account at par (§8.3). This
// and ToAtomic are the only conversions in the codebase: the rail's edge is the
// one place a token amount becomes money, and it is exact, never a float.
func (t Token) ToUSD(atomic *big.Int) (currency.Amount, error) {
	value := new(big.Rat).SetFrac(atomic, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(t.Decimals)), nil))
	amount, err := currency.Parse(value.FloatString(t.Decimals), currency.USD)
	if err != nil {
		return currency.Amount{}, apperr.Internal("settlement: convert a token amount", err)
	}
	return amount, nil
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
