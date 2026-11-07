//go:build integration

package cdp

import (
	"os"
	"testing"

	"github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests call the real CDP, with the credentials in the environment
// (.env.example). Without them they skip, so make check passes on a machine
// that has none. Use a CDP project of its own for development, so these keys
// can never reach a mainnet wallet.
//
// The account names are fixed, so a rerun reuses the same two accounts
// rather than creating new ones, and they cannot collide with a real
// account's, which are named by UUID.

func devnetClient(t *testing.T) *Client {
	t.Helper()
	o := Options{
		APIKeyID:     os.Getenv("CDP_API_KEY_ID"),
		APIKeySecret: os.Getenv("CDP_API_KEY_SECRET"),
		WalletSecret: os.Getenv("CDP_WALLET_SECRET"),
	}
	if o.APIKeyID == "" || o.APIKeySecret == "" || o.WalletSecret == "" {
		t.Skip("CDP_API_KEY_ID, CDP_API_KEY_SECRET and CDP_WALLET_SECRET are not set")
	}
	c, err := New(o)
	require.NoError(t, err)
	return c
}

func TestWallet_OnePerAccount(t *testing.T) {
	c := devnetClient(t)

	first, err := c.SolanaAccount(t.Context(), "cleargate-test-wallet-a")
	require.NoError(t, err)
	again, err := c.SolanaAccount(t.Context(), "cleargate-test-wallet-a")
	require.NoError(t, err)
	other, err := c.SolanaAccount(t.Context(), "cleargate-test-wallet-b")
	require.NoError(t, err)

	assert.Equal(t, first, again, "asking twice for an account's wallet gives the same one")
	assert.NotEqual(t, first, other)
	_, err = solana.PublicKeyFromBase58(first)
	assert.NoError(t, err, "a Solana address")
}

// TestWallet_SignsVerifiableTransaction: CDP signs a transaction whose fee
// payer is the wallet, the signature verifies against the wallet's address,
// and the message is returned byte for byte as sent. Signing needs no funds
// and sends nothing, so the blockhash can be any 32 bytes.
func TestWallet_SignsVerifiableTransaction(t *testing.T) {
	c := devnetClient(t)
	address, err := c.SolanaAccount(t.Context(), "cleargate-test-wallet-a")
	require.NoError(t, err)
	payer := solana.MustPublicKeyFromBase58(address)

	tx, err := solana.NewTransaction(
		[]solana.Instruction{solana.NewInstruction(solana.MemoProgramID,
			solana.AccountMetaSlice{solana.Meta(payer).SIGNER()}, []byte("cleargate test"))},
		solana.Hash{1, 2, 3},
		solana.TransactionPayer(payer))
	require.NoError(t, err)
	unsigned, err := tx.MarshalBinary()
	require.NoError(t, err)

	signedBytes, err := c.SignSolanaTransaction(t.Context(), address, unsigned)
	require.NoError(t, err)

	signed, err := solana.TransactionFromBytes(signedBytes)
	require.NoError(t, err)
	require.NoError(t, signed.VerifySignatures())
	wantMessage, err := tx.Message.MarshalBinary()
	require.NoError(t, err)
	gotMessage, err := signed.Message.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, wantMessage, gotMessage, "CDP signs what it was given and changes nothing")
}

// TestWallet_SignsATransactionSomebodyElsePaysFor is the shape every x402 payment
// has, and the one thing the fakes cannot prove: a v0 transaction whose fee payer
// is another account, with that account's signature slot left empty. CDP must
// fill the wallet's slot, leave the other, and change nothing in the message.
func TestWallet_SignsATransactionSomebodyElsePaysFor(t *testing.T) {
	c := devnetClient(t)
	address, err := c.SolanaAccount(t.Context(), "cleargate-test-wallet-a")
	require.NoError(t, err)
	wallet := solana.MustPublicKeyFromBase58(address)
	feePayer, err := solana.NewRandomPrivateKey()
	require.NoError(t, err)

	tx, err := solana.NewTransaction(
		[]solana.Instruction{solana.NewInstruction(solana.MemoProgramID,
			solana.AccountMetaSlice{solana.Meta(wallet).SIGNER()}, []byte("cleargate test"))},
		solana.Hash{1, 2, 3},
		solana.TransactionPayer(feePayer.PublicKey()))
	require.NoError(t, err)
	tx.Message.SetVersion(solana.MessageVersionV0)
	tx.Signatures = make([]solana.Signature, tx.Message.Header.NumRequiredSignatures)
	unsigned, err := tx.MarshalBinary()
	require.NoError(t, err)

	signedBytes, err := c.SignSolanaTransaction(t.Context(), address, unsigned)
	require.NoError(t, err)

	signed, err := solana.TransactionFromBytes(signedBytes)
	require.NoError(t, err)
	wantMessage, err := tx.Message.MarshalBinary()
	require.NoError(t, err)
	gotMessage, err := signed.Message.MarshalBinary()
	require.NoError(t, err)
	assert.Equal(t, wantMessage, gotMessage, "CDP signs what it was given and changes nothing")

	require.Len(t, signed.Signatures, 2)
	assert.True(t, signed.Signatures[0].IsZero(), "the fee payer's slot stays empty")
	assert.True(t, signed.Signatures[1].Verify(wallet, wantMessage), "the wallet's slot verifies")
}
