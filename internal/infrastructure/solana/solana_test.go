package solana

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	sol "github.com/gagliardetto/solana-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/solana/solanatest"
)

func newClient(t *testing.T) (*solanatest.Fake, *Client) {
	t.Helper()
	fake := solanatest.New(t)
	c, err := New(Options{RPCURL: fake.URL()})
	require.NoError(t, err)
	return fake, c
}

func TestNew_RefusesNoEndpoint(t *testing.T) {
	_, err := New(Options{})
	assert.ErrorIs(t, err, ErrMissingRPCURL)
}

func TestHeights_ReadTheChain(t *testing.T) {
	fake, c := newClient(t)
	fake.SetHeight(2000)
	fake.SetLastValid(2150)

	height, err := c.FinalizedHeight(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint64(2000), height)

	lastValid, err := c.LastValidBlockHeight(context.Background())
	require.NoError(t, err)
	assert.Equal(t, uint64(2150), lastValid)
}

// TestSignatures_PagesNewestFirstWithinItsBounds: the deposit scan walks back
// from the newest to the last one it processed, so before, until and limit
// must all hold.
func TestSignatures_PagesNewestFirstWithinItsBounds(t *testing.T) {
	fake, c := newClient(t)
	owner := solanatest.RandomAddress(t)
	account, err := TokenAccount(owner, solanatest.DevnetUSDC)
	require.NoError(t, err)

	var sigs []string
	for range 4 {
		sigs = append(sigs, fake.Transfer(false, solanatest.Holdings{Owner: owner, Mint: solanatest.DevnetUSDC, After: 1}))
	}
	newest, oldest := sigs[3], sigs[0]

	all, err := c.Signatures(context.Background(), account, Page{})
	require.NoError(t, err)
	require.Len(t, all, 4)
	assert.Equal(t, newest, all[0].Signature)
	assert.Equal(t, oldest, all[3].Signature)

	page, err := c.Signatures(context.Background(), account, Page{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, page, 2)

	below, err := c.Signatures(context.Background(), account, Page{Before: newest, Until: oldest})
	require.NoError(t, err)
	require.Len(t, below, 2)
	assert.Equal(t, sigs[2], below[0].Signature)
	assert.Equal(t, sigs[1], below[1].Signature)

	assert.True(t, fake.OnlyFinalized(), "nothing is read at less than finalized")
}

func TestSignatures_ReportFailureMemoAndTime(t *testing.T) {
	fake, c := newClient(t)
	wallet := solanatest.RandomAddress(t)
	at := time.Now().Add(-time.Minute).Truncate(time.Second)

	fake.Land(wallet, signedTransaction(t, wallet), "the-memo", true, at)

	found, err := c.Signatures(context.Background(), wallet, Page{})
	require.NoError(t, err)
	require.Len(t, found, 1)
	assert.True(t, found[0].Failed)
	assert.Contains(t, found[0].Memo, "the-memo")
	assert.True(t, at.Equal(found[0].BlockTime))
}

// TestTransaction_TokenChangeIsTheNetForOwnerAndMint: a deposit is whatever
// the owner's balance of the one mint gained, not anyone else's or any other
// token's.
func TestTransaction_TokenChangeIsTheNetForOwnerAndMint(t *testing.T) {
	fake, c := newClient(t)
	owner, sender := solanatest.RandomAddress(t), solanatest.RandomAddress(t)
	otherMint := solanatest.RandomAddress(t)

	sig := fake.Transfer(false,
		solanatest.Holdings{Owner: owner, Mint: solanatest.DevnetUSDC, Before: 500_000, After: 2_500_000},
		solanatest.Holdings{Owner: sender, Mint: solanatest.DevnetUSDC, Before: 2_000_000, After: 0},
		solanatest.Holdings{Owner: owner, Mint: otherMint, Before: 0, After: 9},
	)

	tx, err := c.Transaction(context.Background(), sig)
	require.NoError(t, err)
	assert.False(t, tx.Failed)
	assert.Equal(t, "2000000", tx.TokenChange(owner, solanatest.DevnetUSDC).String())
	assert.Equal(t, "-2000000", tx.TokenChange(sender, solanatest.DevnetUSDC).String())
	assert.Equal(t, "9", tx.TokenChange(owner, otherMint).String())
	assert.Equal(t, "0", tx.TokenChange(sender, otherMint).String())
	assert.True(t, fake.OnlyFinalized())
}

func TestTransaction_MissingIsNotFound(t *testing.T) {
	_, c := newClient(t)
	var unknown sol.Signature
	unknown[0] = 1

	_, err := c.Transaction(context.Background(), unknown.String())
	assert.ErrorIs(t, err, ErrTransactionNotFound)
}

// TestTransaction_MessageMatchesWhatWasSigned: settling a payment means finding
// the transaction whose id we never saw, by its message, so a landed
// transaction's message must compare equal to the one Cleargate signed, in
// the v0 format x402 uses, whatever the fee payer added.
func TestTransaction_MessageMatchesWhatWasSigned(t *testing.T) {
	fake, c := newClient(t)
	wallet := solanatest.RandomAddress(t)
	signed := signedTransaction(t, wallet)

	tx, err := c.Transaction(context.Background(), fake.Land(wallet, signed, "", false, time.Now()))
	require.NoError(t, err)

	want, err := MessageOf(signed)
	require.NoError(t, err)
	assert.Equal(t, want, tx.Message)
}

func TestMessageOf_RefusesWhatIsNotATransaction(t *testing.T) {
	_, err := MessageOf("not base64 at all!")
	assert.Error(t, err)
	_, err = MessageOf(base64.StdEncoding.EncodeToString([]byte("short")))
	assert.Error(t, err)
}

func TestTokenAccount_IsDerivedFromOwnerAndMint(t *testing.T) {
	owner := solanatest.RandomAddress(t)

	got, err := TokenAccount(owner, solanatest.DevnetUSDC)
	require.NoError(t, err)
	want, _, err := sol.FindAssociatedTokenAddress(
		sol.MustPublicKeyFromBase58(owner), sol.MustPublicKeyFromBase58(solanatest.DevnetUSDC))
	require.NoError(t, err)
	assert.Equal(t, want.String(), got)

	_, err = TokenAccount("not an address", solanatest.DevnetUSDC)
	assert.Error(t, err)
}

// signedTransaction is a v0 transaction with an empty signature slot for its
// payer, base64 encoded the way x402 carries one.
func signedTransaction(t *testing.T, payer string) string {
	t.Helper()
	tx, err := sol.NewTransaction(
		[]sol.Instruction{sol.NewInstruction(sol.MemoProgramID, sol.AccountMetaSlice{}, []byte("hello"))},
		sol.Hash{9}, sol.TransactionPayer(sol.MustPublicKeyFromBase58(payer)))
	require.NoError(t, err)
	tx.Message.SetVersion(sol.MessageVersionV0)
	tx.Signatures = make([]sol.Signature, tx.Message.Header.NumRequiredSignatures)
	raw, err := tx.MarshalBinary()
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(raw)
}
