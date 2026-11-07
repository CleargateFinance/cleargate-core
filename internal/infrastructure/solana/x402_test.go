package solana

import (
	"context"
	"errors"
	"testing"

	sol "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/x402-foundation/x402/go/mechanisms/svm"
	facilitator "github.com/x402-foundation/x402/go/mechanisms/svm/exact/facilitator"
	"github.com/x402-foundation/x402/go/types"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/solana/solanatest"
)

const devnet = "solana:EtWTRABZaYq6iMfeYKouRu166VU2xqa1"

// keySigner signs the way a custodian does, with a real key.
func keySigner(t *testing.T, key sol.PrivateKey) func(context.Context, []byte) ([]byte, error) {
	t.Helper()
	return func(_ context.Context, raw []byte) ([]byte, error) {
		tx, err := sol.TransactionFromBytes(raw)
		if err != nil {
			return nil, err
		}
		message, err := tx.Message.MarshalBinary()
		if err != nil {
			return nil, err
		}
		signature, err := key.Sign(message)
		if err != nil {
			return nil, err
		}
		for i := range int(tx.Message.Header.NumRequiredSignatures) {
			if tx.Message.AccountKeys[i].Equals(key.PublicKey()) {
				tx.Signatures[i] = signature
			}
		}
		return tx.MarshalBinary()
	}
}

func exactFixture(t *testing.T) (*Client, sol.PrivateKey, ExactTransfer) {
	t.Helper()
	_, c := newClient(t)
	key, err := sol.NewRandomPrivateKey()
	require.NoError(t, err)
	return c, key, ExactTransfer{
		Owner:    key.PublicKey().String(),
		FeePayer: solanatest.RandomAddress(t),
		Network:  devnet,
		Mint:     solanatest.DevnetUSDC,
		PayTo:    solanatest.RandomAddress(t),
		Amount:   "10000",
		Memo:     "the-payment-id",
		Sign:     keySigner(t, key),
	}
}

// facilitatorSigner is the fee payer's side, for the SDK's verifier. Verifying
// needs only to know which addresses it manages.
type facilitatorSigner struct{ feePayer sol.PublicKey }

func (s facilitatorSigner) GetAddresses(context.Context, string) []sol.PublicKey {
	return []sol.PublicKey{s.feePayer}
}

func (facilitatorSigner) SignTransaction(context.Context, *sol.Transaction, sol.PublicKey, string) error {
	return nil
}
func (facilitatorSigner) SimulateTransaction(context.Context, *sol.Transaction, string) error {
	return nil
}
func (facilitatorSigner) SendTransaction(context.Context, *sol.Transaction, string) (sol.Signature, error) {
	return sol.Signature{}, nil
}
func (facilitatorSigner) ConfirmTransaction(context.Context, sol.Signature, string) error {
	return nil
}

var _ svm.FacilitatorSvmSigner = facilitatorSigner{}

// TestSignExact_AFacilitatorAcceptsIt: the whole point of the transaction is
// that a merchant's facilitator will submit it. This runs it through the x402
// SDK's own verifier, which checks the instruction layout, the amount, the
// recipient, the memo and that the fee payer signs nothing else.
func TestSignExact_AFacilitatorAcceptsIt(t *testing.T) {
	c, _, transfer := exactFixture(t)

	signed, err := c.SignExact(context.Background(), transfer)
	require.NoError(t, err)

	requirements := types.PaymentRequirements{
		Scheme: svm.SchemeExact, Network: devnet, Asset: transfer.Mint, Amount: transfer.Amount,
		PayTo: transfer.PayTo, MaxTimeoutSeconds: 60,
		Extra: map[string]any{"feePayer": transfer.FeePayer, "memo": transfer.Memo},
	}
	verdict, err := facilitator.
		NewExactSvmScheme(facilitatorSigner{sol.MustPublicKeyFromBase58(transfer.FeePayer)}).
		Verify(context.Background(), types.PaymentPayload{
			X402Version: 2,
			Payload:     map[string]any{"transaction": signed.Transaction},
			Accepted:    requirements,
		}, requirements, nil)

	require.NoError(t, err)
	assert.True(t, verdict.IsValid, verdict.InvalidReason)
	assert.Equal(t, transfer.Owner, verdict.Payer)
}

// TestSignExact_SignsExactlyWhatWasAsked reads the transaction back: the wallet
// signed, the fee payer's slot is open for the merchant's facilitator, and the
// transfer and memo are the ones asked for.
func TestSignExact_SignsExactlyWhatWasAsked(t *testing.T) {
	c, key, transfer := exactFixture(t)

	signed, err := c.SignExact(context.Background(), transfer)
	require.NoError(t, err)
	assert.Equal(t, uint64(1150), signed.LastValidBlockHeight)

	tx, err := decodeTx(signed.Transaction)
	require.NoError(t, err)
	message, err := tx.Message.MarshalBinary()
	require.NoError(t, err)

	require.Len(t, tx.Signatures, 2)
	assert.True(t, tx.Message.AccountKeys[0].Equals(sol.MustPublicKeyFromBase58(transfer.FeePayer)), "the merchant's facilitator pays the fee")
	assert.True(t, tx.Signatures[0].IsZero(), "the fee payer's slot is left for it")
	assert.True(t, tx.Message.AccountKeys[1].Equals(key.PublicKey()))
	assert.True(t, tx.Signatures[1].Verify(key.PublicKey(), message))
	assert.Equal(t, message, signed.Message, "the message reported is the one signed")

	var memo string
	var transferred *token.TransferChecked
	for _, ix := range tx.Message.Instructions {
		program, err := tx.ResolveProgramIDIndex(ix.ProgramIDIndex)
		require.NoError(t, err)
		accounts, err := ix.ResolveInstructionAccounts(&tx.Message)
		require.NoError(t, err)
		switch {
		case program.Equals(sol.MemoProgramID):
			memo = string(ix.Data)
		case program.Equals(sol.TokenProgramID):
			decoded, err := token.DecodeInstruction(accounts, ix.Data)
			require.NoError(t, err)
			transferred = decoded.Impl.(*token.TransferChecked)
		}
	}
	assert.Equal(t, "the-payment-id", memo)
	require.NotNil(t, transferred)
	assert.Equal(t, uint64(10000), *transferred.Amount)
	assert.Equal(t, uint8(6), *transferred.Decimals, "from the mint on-chain")

	want, err := TokenAccount(transfer.PayTo, transfer.Mint)
	require.NoError(t, err)
	assert.Equal(t, want, transferred.GetDestinationAccount().PublicKey.String())
}

// TestSignExact_RefusesACustodianThatSignedSomethingElse: this is the last point
// before a transaction leaves for a merchant.
func TestSignExact_RefusesACustodianThatSignedSomethingElse(t *testing.T) {
	c, key, transfer := exactFixture(t)
	other, err := sol.NewRandomPrivateKey()
	require.NoError(t, err)

	for name, tc := range map[string]struct {
		sign func(context.Context, []byte) ([]byte, error)
		want string
	}{
		"another key's signature": {sign: func(_ context.Context, raw []byte) ([]byte, error) {
			tx, err := sol.TransactionFromBytes(raw)
			if err != nil {
				return nil, err
			}
			message, err := tx.Message.MarshalBinary()
			if err != nil {
				return nil, err
			}
			if tx.Signatures[1], err = other.Sign(message); err != nil {
				return nil, err
			}
			return tx.MarshalBinary()
		}, want: "does not verify"},
		"a changed message": {sign: func(ctx context.Context, raw []byte) ([]byte, error) {
			tx, err := sol.TransactionFromBytes(raw)
			if err != nil {
				return nil, err
			}
			tx.Message.RecentBlockhash = sol.Hash{42}
			return keySigner(t, key)(ctx, mustMarshal(t, tx))
		}, want: "changed the transaction"},
		"an unreadable answer": {sign: func(context.Context, []byte) ([]byte, error) {
			return []byte("nope"), nil
		}, want: "decode signed transaction"},
		"a refusal": {sign: func(context.Context, []byte) ([]byte, error) {
			return nil, errors.New("custodian down")
		}, want: "custodian down"},
	} {
		t.Run(name, func(t *testing.T) {
			transfer.Sign = tc.sign
			_, err := c.SignExact(context.Background(), transfer)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestSignExact_RefusesAnUnknownMint(t *testing.T) {
	c, _, transfer := exactFixture(t)
	transfer.Mint = solanatest.RandomAddress(t)

	_, err := c.SignExact(context.Background(), transfer)

	assert.Error(t, err)
}

func mustMarshal(t *testing.T, tx *sol.Transaction) []byte {
	t.Helper()
	raw, err := tx.MarshalBinary()
	require.NoError(t, err)
	return raw
}

func decodeTx(signed string) (*sol.Transaction, error) {
	return svm.DecodeTransaction(signed)
}
