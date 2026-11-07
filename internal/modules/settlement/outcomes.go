package settlement

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

const (
	// expiryMargin is how far past a transaction's last valid height the chain
	// must be before it counts as unable to land. Heights and history can come
	// from different nodes behind one endpoint, so a transaction that landed
	// may not show yet on the node asked. Releasing funds for a payment that
	// then appears is the one mistake here that loses money, so this is about a
	// minute of blocks of doubt, at the price of a minute's delay.
	expiryMargin = 150

	// historySlack is how far before the oldest pending payment the history is
	// read, for the clock difference between Cleargate and the chain.
	historySlack = 5 * time.Minute

	// maxHistoryPages bounds one wallet's walk through its history. Hitting it
	// means the search was incomplete, which expires nothing.
	maxHistoryPages = 20
)

// TransferOutcome is what the chain says became of a signed transfer.
type TransferOutcome string

const (
	// TransferPending could still land.
	TransferPending TransferOutcome = "pending"
	// TransferLanded landed and succeeded.
	TransferLanded TransferOutcome = "landed"
	// TransferFailed landed and failed, so it moved nothing and never will.
	TransferFailed TransferOutcome = "failed"
	// TransferExpired did not land, and its blockhash has gone past use.
	TransferExpired TransferOutcome = "expired"
)

// PendingTransfer is a signed transfer waiting on the chain.
type PendingTransfer struct {
	PaymentID            id.PaymentID
	WalletAddress        string
	Memo                 string
	Transaction          string
	LastValidBlockHeight uint64
	DecidedAt            time.Time
}

// TransferObserver reports what became of signed transfers (Task 7.7). Like the
// deposit watcher it runs in the worker and needs no custodian.
type TransferObserver struct {
	chain ChainReader
}

// NewTransferObserver builds a TransferObserver.
func NewTransferObserver(chain ChainReader) (*TransferObserver, error) {
	if chain == nil {
		return nil, fmt.Errorf("%w: chain", ErrMissingDependency)
	}
	return &TransferObserver{chain: chain}, nil
}

// Outcomes says what became of each transfer. A wallet whose history could not
// be read is left out, and its transfers come back unresolved next time; so is
// everything when the height cannot be read. Failing to read the chain must
// never look like a transfer that did not land.
func (o *TransferObserver) Outcomes(ctx context.Context, pending []PendingTransfer) (map[id.PaymentID]TransferOutcome, error) {
	byWallet := map[string][]PendingTransfer{}
	for _, p := range pending {
		byWallet[p.WalletAddress] = append(byWallet[p.WalletAddress], p)
	}

	out := make(map[id.PaymentID]TransferOutcome, len(pending))
	var failures []error
	for wallet, transfers := range byWallet {
		if err := o.walletOutcomes(ctx, wallet, transfers, out); err != nil {
			failures = append(failures, fmt.Errorf("wallet %s: %w", wallet, err))
		}
	}
	return out, errors.Join(failures...)
}

// walletOutcomes resolves one wallet's transfers.
//
// The height is read before the history, and that order is the safety argument.
// If the chain has passed a transaction's last valid height at the moment of
// that read, the transaction is either already in a finalized block or never
// will be, so a later read of the history sees it if it exists.
func (o *TransferObserver) walletOutcomes(
	ctx context.Context, wallet string, transfers []PendingTransfer, out map[id.PaymentID]TransferOutcome,
) error {
	height, err := o.chain.FinalizedHeight(ctx)
	if err != nil {
		return err
	}

	oldest := transfers[0].DecidedAt
	for _, t := range transfers {
		if t.DecidedAt.Before(oldest) {
			oldest = t.DecidedAt
		}
	}
	history, complete, err := o.history(ctx, wallet, oldest.Add(-historySlack))
	if err != nil {
		return err
	}

	found := make(map[id.PaymentID]TransferOutcome, len(transfers))
	for _, t := range transfers {
		outcome, err := o.find(ctx, wallet, t, history)
		if err != nil {
			return err
		}
		switch {
		case outcome != TransferPending:
		case complete && height > t.LastValidBlockHeight+expiryMargin:
			outcome = TransferExpired
		}
		found[t.PaymentID] = outcome
	}
	// Only now: a failure part-way resolves none of this wallet's transfers, so
	// nothing is concluded from a read of the chain that did not finish.
	for paymentID, outcome := range found {
		out[paymentID] = outcome
	}
	return nil
}

// history reads the wallet's transactions back to since, newest first. complete
// is false if it stopped at the page limit before getting there.
func (o *TransferObserver) history(ctx context.Context, wallet string, since time.Time) ([]ChainSignature, bool, error) {
	var all []ChainSignature
	before := ""
	for range maxHistoryPages {
		page, err := o.chain.Signatures(ctx, wallet, ChainPage{Before: before, Limit: depositPageSize})
		if err != nil {
			return nil, false, err
		}
		all = append(all, page...)
		if len(page) < depositPageSize {
			return all, true, nil
		}
		last := page[len(page)-1]
		if !last.BlockTime.IsZero() && last.BlockTime.Before(since) {
			return all, true, nil
		}
		before = last.Signature
	}
	return all, false, nil
}

// find looks for one transfer in the wallet's history.
//
// The memo narrows the search and the message decides it. A merchant can reuse
// its memo across payments, so the memo alone could settle the wrong one, while
// the message is the bytes the wallet signed. The transaction's id cannot be
// used: it is the fee payer's signature, which Cleargate never sees.
func (o *TransferObserver) find(ctx context.Context, wallet string, t PendingTransfer, history []ChainSignature) (TransferOutcome, error) {
	want, err := o.chain.MessageOf(t.Transaction)
	if err != nil {
		return "", err
	}
	for _, sig := range history {
		if !strings.Contains(sig.Memo, t.Memo) {
			continue
		}
		tx, err := o.chain.Transaction(ctx, sig.Signature, wallet)
		if err != nil {
			return "", err
		}
		if !bytes.Equal(tx.Message, want) {
			continue
		}
		if tx.Failed {
			return TransferFailed, nil
		}
		return TransferLanded, nil
	}
	return TransferPending, nil
}
