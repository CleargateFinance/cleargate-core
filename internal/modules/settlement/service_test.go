package settlement

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// testToken is devnet USDC.
var testToken = Token{
	Symbol: "USDC", Network: "solana:EtWTRABZaYq6iMfeYKouRu166VU2xqa1",
	Mint: "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU", Decimals: 6,
}

// everyoneIsAMember lets every request through, for tests about wallets.
type everyoneIsAMember struct{}

func (everyoneIsAMember) RequireMember(context.Context, id.UserID, id.AccountID) error { return nil }

const (
	addrA = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"
	addrB = "4Nd1mBQtrMJVYVfKf2PJy9NZUZdTAsp7D4xWLs4gDB4T"
)

// fakeProvider hands out one address per account and counts what it is
// asked, so a test can tell whether the provider was reached at all.
type fakeProvider struct {
	addresses map[id.AccountID]string
	creates   int
	signed    []string
	err       error
}

func (p *fakeProvider) Chain() Chain { return ChainSolana }
func (p *fakeProvider) Name() string { return "cdp" }

func (p *fakeProvider) Create(_ context.Context, accountID id.AccountID) (string, error) {
	p.creates++
	return p.addresses[accountID], p.err
}

func (p *fakeProvider) Sign(_ context.Context, address string, tx []byte) ([]byte, error) {
	p.signed = append(p.signed, address)
	return append([]byte("signed:"), tx...), p.err
}

// fakeRepo keeps wallets in memory. saveAs, when set, is what Save reports
// as already recorded, standing in for a row another caller wrote first.
type fakeRepo struct {
	Repository // Anything a test does not fake panics rather than passes.

	wallets map[id.AccountID]Wallet
	saveAs  *Wallet
	err     error
}

func (r *fakeRepo) Wallet(_ context.Context, accountID id.AccountID, _ Chain) (Wallet, error) {
	if r.err != nil {
		return Wallet{}, r.err
	}
	w, ok := r.wallets[accountID]
	if !ok {
		return Wallet{}, ErrNoWallet
	}
	return w, nil
}

func (r *fakeRepo) Save(_ context.Context, w Wallet) (Wallet, error) {
	if r.saveAs != nil {
		return *r.saveAs, nil
	}
	if r.wallets == nil {
		r.wallets = map[id.AccountID]Wallet{}
	}
	r.wallets[w.AccountID] = w
	return w, nil
}

func TestAddress_CreatesTheWalletOnFirstUseOnly(t *testing.T) {
	provider := &fakeProvider{addresses: map[id.AccountID]string{"acct": addrA}}
	repo := &fakeRepo{}
	svc := NewService(repo, provider)

	first, err := svc.Address(context.Background(), "acct")
	require.NoError(t, err)
	second, err := svc.Address(context.Background(), "acct")
	require.NoError(t, err)

	assert.Equal(t, addrA, first)
	assert.Equal(t, first, second)
	assert.Equal(t, 1, provider.creates, "the second call reads the record")
	assert.Equal(t, Wallet{AccountID: "acct", Chain: ChainSolana, Provider: "cdp", Address: addrA},
		repo.wallets["acct"])
}

func TestAddress_RefusesAProviderThatDisagreesWithTheRecord(t *testing.T) {
	provider := &fakeProvider{addresses: map[id.AccountID]string{"acct": addrA}}
	repo := &fakeRepo{saveAs: &Wallet{AccountID: "acct", Chain: ChainSolana, Address: addrB}}

	_, err := NewService(repo, provider).Address(context.Background(), "acct")

	assert.ErrorIs(t, err, errWalletMismatch)
	assert.Equal(t, apperr.KindInternal, apperr.KindOf(err))
}

func TestAddress_ProviderFailureRecordsNothing(t *testing.T) {
	provider := &fakeProvider{err: errors.New("cdp: 503")}
	repo := &fakeRepo{}

	_, err := NewService(repo, provider).Address(context.Background(), "acct")

	require.Error(t, err)
	assert.Empty(t, repo.wallets)
}

func TestAddress_StorageFailureNeverReachesTheProvider(t *testing.T) {
	provider := &fakeProvider{}
	broken := errors.New("connection refused")

	_, err := NewService(&fakeRepo{err: broken}, provider).Address(context.Background(), "acct")

	assert.ErrorIs(t, err, broken)
	assert.Zero(t, provider.creates, "a read that failed is not a wallet that is missing")
}

func TestSignTransaction_SignsWithTheRecordedWallet(t *testing.T) {
	provider := &fakeProvider{}
	repo := &fakeRepo{wallets: map[id.AccountID]Wallet{"acct": {AccountID: "acct", Chain: ChainSolana, Address: addrA}}}

	signed, err := NewService(repo, provider).SignTransaction(context.Background(), "acct", []byte("tx"))

	require.NoError(t, err)
	assert.Equal(t, []byte("signed:tx"), signed)
	assert.Equal(t, []string{addrA}, provider.signed)
}

func TestSignTransaction_NeverCreatesAWallet(t *testing.T) {
	provider := &fakeProvider{addresses: map[id.AccountID]string{"acct": addrA}}

	_, err := NewService(&fakeRepo{}, provider).SignTransaction(context.Background(), "acct", []byte("tx"))

	assert.ErrorIs(t, err, ErrNoWallet)
	assert.Zero(t, provider.creates)
	assert.Empty(t, provider.signed)
}

func TestNew_RefusesAMissingWalletProvider(t *testing.T) {
	_, err := New(Deps{})
	assert.ErrorIs(t, err, ErrMissingDependency)

	_, err = New(Deps{DB: &database.DB{}})
	assert.ErrorContains(t, err, "wallet provider")
}

// TestDepositAddress_AsksAboutTheMemberBeforeTheCustodian: a stranger's request
// must not create a wallet at the custodian, which costs nothing to ask for
// and is never deleted.
func TestDepositAddress_AsksAboutTheMemberBeforeTheCustodian(t *testing.T) {
	refused := apperr.Forbidden("no")
	provider := &fakeProvider{addresses: map[id.AccountID]string{"acct": addrA}}
	svc := NewService(&fakeRepo{wallets: map[id.AccountID]Wallet{}}, provider)
	svc.accounts, svc.token = refusingAccounts{refused}, testToken

	_, err := svc.DepositAddress(context.Background(), "user", "acct")

	assert.ErrorIs(t, err, refused)
	assert.Zero(t, provider.creates)
}

func TestDepositAddress_NamesTheAddressAndTheToken(t *testing.T) {
	provider := &fakeProvider{addresses: map[id.AccountID]string{"acct": addrA}}
	svc := NewService(&fakeRepo{wallets: map[id.AccountID]Wallet{}}, provider)
	svc.accounts, svc.token = everyoneIsAMember{}, testToken

	got, err := svc.DepositAddress(context.Background(), "user", "acct")

	require.NoError(t, err)
	assert.Equal(t, addrA, got.Address)
	assert.Equal(t, testToken, got.Token)
}

type refusingAccounts struct{ err error }

func (r refusingAccounts) RequireMember(context.Context, id.UserID, id.AccountID) error { return r.err }

// fakeRail records the transfer it was asked for and signs nothing.
type fakeRail struct {
	got RailTransfer
	err error
}

func (r *fakeRail) Transfer(_ context.Context, t RailTransfer) (RailSigned, error) {
	r.got = t
	return RailSigned{Transaction: "c2lnbmVk", LastValidBlockHeight: 1150}, r.err
}
