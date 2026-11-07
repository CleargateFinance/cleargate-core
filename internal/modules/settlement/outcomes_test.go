package settlement

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// fakeChain serves a wallet's history, newest first, one page at a time.
type fakeChain struct {
	ChainReader // Anything a test does not fake panics rather than passes.

	height       uint64
	heightErr    error
	history      []ChainSignature
	transactions map[string]ChainTransaction
	pages        int
}

func (c *fakeChain) FinalizedHeight(context.Context) (uint64, error) { return c.height, c.heightErr }

func (c *fakeChain) MessageOf(signed string) ([]byte, error) { return []byte(signed), nil }

func (c *fakeChain) Signatures(_ context.Context, _ string, p ChainPage) ([]ChainSignature, error) {
	c.pages++
	start := 0
	if p.Before != "" {
		for i, s := range c.history {
			if s.Signature == p.Before {
				start = i + 1
			}
		}
	}
	end := min(start+p.Limit, len(c.history))
	return c.history[start:end], nil
}

func (c *fakeChain) Transaction(_ context.Context, signature, _ string) (ChainTransaction, error) {
	tx, ok := c.transactions[signature]
	if !ok {
		return ChainTransaction{}, errors.New("not found")
	}
	return tx, nil
}

func pendingTransfer(paymentID, memo, tx string) PendingTransfer {
	return PendingTransfer{
		PaymentID: id.PaymentID(paymentID), WalletAddress: addrA, Memo: memo, Transaction: tx,
		LastValidBlockHeight: 1000, DecidedAt: time.Now(),
	}
}

func outcomeOf(t *testing.T, chain *fakeChain, p PendingTransfer) TransferOutcome {
	t.Helper()
	o, err := NewTransferObserver(chain)
	require.NoError(t, err)
	got, err := o.Outcomes(context.Background(), []PendingTransfer{p})
	require.NoError(t, err)
	return got[p.PaymentID]
}

// TestOutcomes_HeightDecidesExpiry: nothing is expired until the chain is past
// the last valid height and the margin, because releasing funds for a payment
// that then lands is the one mistake here that loses money.
func TestOutcomes_HeightDecidesExpiry(t *testing.T) {
	p := pendingTransfer("p1", "m1", "tx1")

	for height, want := range map[uint64]TransferOutcome{
		999:                     TransferPending,
		1000:                    TransferPending,
		1000 + expiryMargin:     TransferPending,
		1000 + expiryMargin + 1: TransferExpired,
	} {
		assert.Equal(t, want, outcomeOf(t, &fakeChain{height: height}, p), "height %d", height)
	}
}

func TestOutcomes_ALandedTransferBeatsAnExpiredHeight(t *testing.T) {
	chain := &fakeChain{
		height:       1_000_000,
		history:      []ChainSignature{{Signature: "s1", Memo: "[2] m1"}},
		transactions: map[string]ChainTransaction{"s1": {Message: []byte("tx1")}},
	}

	assert.Equal(t, TransferLanded, outcomeOf(t, chain, pendingTransfer("p1", "m1", "tx1")))
}

func TestOutcomes_AFailedTransferIsFinal(t *testing.T) {
	chain := &fakeChain{
		height:       500,
		history:      []ChainSignature{{Signature: "s1", Memo: "[2] m1", Failed: true}},
		transactions: map[string]ChainTransaction{"s1": {Message: []byte("tx1"), Failed: true}},
	}

	assert.Equal(t, TransferFailed, outcomeOf(t, chain, pendingTransfer("p1", "m1", "tx1")))
}

// TestOutcomes_AMemoAloneProvesNothing: a merchant can set the same memo on every
// payment, so another transaction carrying it must not settle this one.
func TestOutcomes_AMemoAloneProvesNothing(t *testing.T) {
	chain := &fakeChain{
		height:       500,
		history:      []ChainSignature{{Signature: "s1", Memo: "[8] order-77"}},
		transactions: map[string]ChainTransaction{"s1": {Message: []byte("somebody else's")}},
	}

	assert.Equal(t, TransferPending, outcomeOf(t, chain, pendingTransfer("p1", "order-77", "tx1")))
}

// TestOutcomes_AnIncompleteSearchExpiresNothing: a wallet with more history than
// one sweep reads has not been shown to lack the transaction.
func TestOutcomes_AnIncompleteSearchExpiresNothing(t *testing.T) {
	chain := &fakeChain{height: 1_000_000}
	for i := range depositPageSize * (maxHistoryPages + 1) {
		chain.history = append(chain.history, ChainSignature{Signature: fmt.Sprintf("s%d", i), BlockTime: time.Now()})
	}

	assert.Equal(t, TransferPending, outcomeOf(t, chain, pendingTransfer("p1", "m1", "tx1")))
	assert.Equal(t, maxHistoryPages, chain.pages)
}

// TestOutcomes_StopsReadingBeforeTheOldestPayment: a busy wallet's whole history
// is not read every 30 seconds, and stopping there still counts as complete.
func TestOutcomes_StopsReadingBeforeTheOldestPayment(t *testing.T) {
	chain := &fakeChain{height: 1_000_000}
	old := time.Now().Add(-time.Hour)
	for i := range depositPageSize * 5 {
		chain.history = append(chain.history, ChainSignature{Signature: fmt.Sprintf("s%d", i), BlockTime: old})
	}

	assert.Equal(t, TransferExpired, outcomeOf(t, chain, pendingTransfer("p1", "m1", "tx1")))
	assert.Equal(t, 1, chain.pages)
}

// TestOutcomes_AFailedReadConcludesNothing: not being able to read the chain must
// never look like a transfer that did not land.
func TestOutcomes_AFailedReadConcludesNothing(t *testing.T) {
	o, err := NewTransferObserver(&fakeChain{heightErr: errors.New("node down")})
	require.NoError(t, err)

	got, err := o.Outcomes(context.Background(), []PendingTransfer{pendingTransfer("p1", "m1", "tx1")})

	assert.ErrorContains(t, err, "node down")
	assert.Empty(t, got)
}

func TestOutcomes_AListedTransactionItCannotReadConcludesNothing(t *testing.T) {
	o, err := NewTransferObserver(&fakeChain{
		height:  1_000_000,
		history: []ChainSignature{{Signature: "s1", Memo: "[2] m1"}},
	})
	require.NoError(t, err)

	got, err := o.Outcomes(context.Background(), []PendingTransfer{pendingTransfer("p1", "m1", "tx1")})

	assert.ErrorContains(t, err, "not found")
	assert.Empty(t, got)
}
