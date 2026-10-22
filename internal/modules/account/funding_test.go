//go:build integration

package account

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// stubFunding stands in for the ledger until the funding flow is wired.
type stubFunding struct{ funded string }

func (s stubFunding) FundedToDate(context.Context, id.PrincipalID) (currency.Amount, error) {
	if s.funded == "" {
		return currency.Parse("0", currency.USDC)
	}
	return currency.Parse(s.funded, currency.USDC)
}

// fundingFixture is an account with a principal, and a service whose funding
// history is stubbed to a controllable total.
type fundingFixture struct {
	*authFixture
	principal id.PrincipalID
}

func newFundingFixture(t *testing.T, fundedToDate string) *fundingFixture {
	t.Helper()
	ctx := context.Background()

	db := fixtures.NewTestDB(t)
	redis := fixtures.NewTestCache(t)
	repo := NewPostgresRepository(db.DB)
	mailer := newCaptureMailer()

	svc := NewService(repo, cheapHasher(), redis.Client, mailer, testSealer(t),
		NewCacheThrottle(redis.Client, redis.Client, nil), nil, nil, Options{
			FundingHistory: stubFunding{funded: fundedToDate},
		})

	f := &fundingFixture{
		authFixture: &authFixture{db: db, cache: redis, svc: svc, repo: repo, mailer: mailer},
	}

	userID := f.signupAndVerify(t, "funder@example.com")
	user, err := repo.UserByID(ctx, userID)
	require.NoError(t, err)
	f.principal = user.PrincipalID

	return f
}

// CAIP-2 network ids.
const (
	baseChain    = "eip155:8453"
	solanaDevnet = "solana:EtWTRABZaYq6iMfeYKouRu166VU2xqa1"
)

// screen records a verdict for an address on Base.
func (f *fundingFixture) screen(t *testing.T, address string, status ScreeningStatus) {
	t.Helper()
	f.screenOn(t, baseChain, address, status)
}

// screenOn records a verdict for an address on any chain.
func (f *fundingFixture) screenOn(t *testing.T, chain, address string, status ScreeningStatus) {
	t.Helper()

	now := time.Now().UTC()
	_, err := f.svc.RecordScreening(context.Background(), ScreenedAddress{
		PrincipalID: f.principal,
		Chain:       chain,
		Address:     address,
		Status:      status,
		Provider:    "cdp",
		Reference:   "scr-1",
		ScreenedAt:  &now,
	})
	require.NoError(t, err)
}

// verify puts the principal through identity verification, which is the only
// thing that lifts the basic tier's cap and its return-to-source restriction.
func (f *fundingFixture) verify(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	_, err := f.db.Exec(ctx, `
		UPDATE principal_identity
		SET kyc_status = 'verified', kyc_verified_at = now(),
		    kyc_provider = 'coinbase', kyc_reference = 'kyc-1'
		WHERE principal_id = $1`, string(f.principal))
	require.NoError(t, err)

	require.NoError(t, f.repo.SetVerificationTier(ctx, f.principal, TierVerified))
}

func usd(t *testing.T, v string) currency.Amount {
	t.Helper()
	a, err := currency.Parse(v, currency.USDC)
	require.NoError(t, err)
	return a
}

// TestUnverifiedPrincipalCanFundSmallAmountWithoutKyc is the trial path.
//
// Somebody testing the system with fifty dollars of stablecoin gives us an
// email address and nothing else. No identity documents, no verification, and
// the money still moves — because the control that makes this safe is screening
// the address it came from, not knowing who typed it.
func TestUnverifiedPrincipalCanFundSmallAmountWithoutKyc(t *testing.T) {
	ctx := context.Background()
	f := newFundingFixture(t, "0")

	// The principal is unverified, which is the starting state.
	tier, err := f.repo.VerificationTier(ctx, f.principal)
	require.NoError(t, err)
	require.Equal(t, TierBasic, tier)

	f.screen(t, "0xabc", ScreeningClear)

	assert.NoError(t, f.svc.RequireFundingAllowed(ctx, FundingRequest{
		PrincipalID: f.principal,
		Amount:      usd(t, "50"),
		Chain:       baseChain,
		Address:     "0xabc",
	}), "a fifty dollar trial must not require identity documents")
}

// TestUnverifiedFundingStillRequiresScreening is what keeps the trial from
// being reckless. Screening is not KYC, and dropping it in the name of
// onboarding speed is the version of this decision that ends in an enforcement
// action.
func TestUnverifiedFundingStillRequiresScreening(t *testing.T) {
	ctx := context.Background()
	f := newFundingFixture(t, "0")

	request := FundingRequest{
		PrincipalID: f.principal,
		Amount:      usd(t, "50"),
		Chain:       baseChain,
		Address:     "0xdirty",
	}

	// No verdict yet: a wait, not a bypass.
	assert.ErrorIs(t, f.svc.RequireFundingAllowed(ctx, request), ErrAddressNotScreened)

	// A bad verdict: refused, and flagged and rejected answer identically.
	f.screen(t, "0xdirty", ScreeningFlagged)
	assert.ErrorIs(t, f.svc.RequireFundingAllowed(ctx, request), ErrAddressRejected)

	f.screen(t, "0xdirty", ScreeningRejected)
	assert.ErrorIs(t, f.svc.RequireFundingAllowed(ctx, request), ErrAddressRejected)
}

// TestSpendLimitFollowsTierNotFundingMethod is the rule the whole design turns
// on: what you may move is decided by your verification level, never by whether
// the money arrived by card or on-chain.
func TestSpendLimitFollowsTierNotFundingMethod(t *testing.T) {
	ctx := context.Background()

	t.Run("basic tier is capped", func(t *testing.T) {
		f := newFundingFixture(t, "60")
		f.screen(t, "0xabc", ScreeningClear)

		// 60 already funded, 50 more would pass the 100 basic cap.
		err := f.svc.RequireFundingAllowed(ctx, FundingRequest{
			PrincipalID: f.principal, Amount: usd(t, "50"),
			Chain: baseChain, Address: "0xabc",
		})
		assert.ErrorIs(t, err, ErrFundingCapExceeded)

		// Just under it still passes.
		assert.NoError(t, f.svc.RequireFundingAllowed(ctx, FundingRequest{
			PrincipalID: f.principal, Amount: usd(t, "40"),
			Chain: baseChain, Address: "0xabc",
		}))
	})

	t.Run("the same amount by fiat is capped identically", func(t *testing.T) {
		f := newFundingFixture(t, "60")

		// No chain or address: a fiat on-ramp deposit, where the on-ramp has
		// already done its own due diligence and there is nothing to screen.
		// The cap is unchanged, which is the point.
		err := f.svc.RequireFundingAllowed(ctx, FundingRequest{
			PrincipalID: f.principal, Amount: usd(t, "50"),
		})
		assert.ErrorIs(t, err, ErrFundingCapExceeded)
	})

	t.Run("verifying lifts the cap", func(t *testing.T) {
		f := newFundingFixture(t, "60")
		f.screen(t, "0xabc", ScreeningClear)

		f.verify(t)

		assert.NoError(t, f.svc.RequireFundingAllowed(ctx, FundingRequest{
			PrincipalID: f.principal, Amount: usd(t, "5000"),
			Chain: baseChain, Address: "0xabc",
		}))
	})
}

// TestTierAboveBasicRequiresVerifiedKyc checks the database refuses an
// entitlement that was never established, regardless of what a service asks for.
func TestTierAboveBasicRequiresVerifiedKyc(t *testing.T) {
	ctx := context.Background()
	f := newFundingFixture(t, "0")

	// kyc_status is still 'none', so the tier cannot be raised.
	err := f.repo.SetVerificationTier(ctx, f.principal, TierVerified)
	assert.Error(t, err, "a tier above basic without verified KYC must be refused")

	tier, err := f.repo.VerificationTier(ctx, f.principal)
	require.NoError(t, err)
	assert.Equal(t, TierBasic, tier)
}

// TestUnverifiedWithdrawalMustReturnToSource is the control that makes the
// no-verification tier safe to offer.
//
// Without it, funding anonymously and withdrawing elsewhere is a laundering
// route, and repeated across accounts it is textbook structuring. With it, an
// unverified account cannot move value between two addresses — only return what
// it brought.
func TestUnverifiedWithdrawalMustReturnToSource(t *testing.T) {
	ctx := context.Background()
	f := newFundingFixture(t, "50")

	f.screen(t, "0xsource", ScreeningClear)
	f.screen(t, "0xelsewhere", ScreeningClear)

	// Money arrived from 0xsource.
	require.NoError(t, f.svc.RecordFunding(ctx, FundingRequest{
		PrincipalID: f.principal, Amount: usd(t, "50"),
		Chain: baseChain, Address: "0xsource",
	}, time.Now().UTC()))

	// Back to where it came from: allowed.
	assert.NoError(t, f.svc.RequireWithdrawalAllowed(ctx, WithdrawalRequest{
		PrincipalID: f.principal, Amount: usd(t, "20"),
		Chain: baseChain, Address: "0xsource",
	}))

	// Somewhere else, even though that address is perfectly clean: refused.
	assert.ErrorIs(t, f.svc.RequireWithdrawalAllowed(ctx, WithdrawalRequest{
		PrincipalID: f.principal, Amount: usd(t, "20"),
		Chain: baseChain, Address: "0xelsewhere",
	}), ErrWithdrawalNotToSource)
}

// TestVerifiedPrincipalMayWithdrawAnywhere is the honest trade being offered:
// tell us who you are and you may send money where you like.
func TestVerifiedPrincipalMayWithdrawAnywhere(t *testing.T) {
	ctx := context.Background()
	f := newFundingFixture(t, "50")

	f.screen(t, "0xelsewhere", ScreeningClear)
	f.verify(t)

	assert.NoError(t, f.svc.RequireWithdrawalAllowed(ctx, WithdrawalRequest{
		PrincipalID: f.principal, Amount: usd(t, "20"),
		Chain: baseChain, Address: "0xelsewhere",
	}))
}

// TestWithdrawalScreensDestinationAtEveryTier checks that verification was
// never a claim about where somebody sends money.
func TestWithdrawalScreensDestinationAtEveryTier(t *testing.T) {
	ctx := context.Background()
	f := newFundingFixture(t, "50")

	f.screen(t, "0xsanctioned", ScreeningRejected)
	f.verify(t)

	assert.ErrorIs(t, f.svc.RequireWithdrawalAllowed(ctx, WithdrawalRequest{
		PrincipalID: f.principal, Amount: usd(t, "20"),
		Chain: baseChain, Address: "0xsanctioned",
	}), ErrAddressRejected)
}

// TestAddressComparisonIgnoresCase covers checksummed EVM addresses, which are
// routinely written in mixed case. Two spellings of one address must never look
// like two addresses to a return-to-source check.
func TestAddressComparisonIgnoresCase(t *testing.T) {
	ctx := context.Background()
	f := newFundingFixture(t, "50")

	f.screen(t, "0xabcdef", ScreeningClear)
	require.NoError(t, f.svc.RecordFunding(ctx, FundingRequest{
		PrincipalID: f.principal, Amount: usd(t, "50"),
		Chain: baseChain, Address: "0xAbCdEf",
	}, time.Now().UTC()))

	assert.NoError(t, f.svc.RequireWithdrawalAllowed(ctx, WithdrawalRequest{
		PrincipalID: f.principal, Amount: usd(t, "20"),
		Chain: baseChain, Address: "0xABCDEF",
	}))
}

// TestSolanaAddressKeepsItsCase: Solana addresses are case-sensitive base58,
// so a lowercased one is a different address.
func TestSolanaAddressKeepsItsCase(t *testing.T) {
	ctx := context.Background()
	f := newFundingFixture(t, "0")

	const address = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"
	f.screenOn(t, solanaDevnet, address, ScreeningClear)

	request := FundingRequest{
		PrincipalID: f.principal,
		Amount:      usd(t, "50"),
		Chain:       solanaDevnet,
		Address:     address,
	}
	require.NoError(t, f.svc.RequireFundingAllowed(ctx, request))

	request.Address = strings.ToLower(address)
	assert.ErrorIs(t, f.svc.RequireFundingAllowed(ctx, request), ErrAddressNotScreened)
}

func TestScreeningRefusesChainThatIsNotCAIP2(t *testing.T) {
	f := newFundingFixture(t, "0")

	_, err := f.svc.RecordScreening(context.Background(), ScreenedAddress{
		PrincipalID: f.principal,
		Chain:       "base",
		Address:     "0xabc",
		Status:      ScreeningPending,
	})
	require.Error(t, err)
	assert.Equal(t, apperr.KindInvalid, apperr.KindOf(err))
}

// TestFundingRefusedWhenHistoryNotWired checks the gate fails closed.
//
// Assuming zero funded-to-date would turn a missing dependency into an
// unbounded cap, silently, on the one path where that matters most.
func TestFundingRefusedWhenHistoryNotWired(t *testing.T) {
	ctx := context.Background()

	db := fixtures.NewTestDB(t)
	redis := fixtures.NewTestCache(t)
	repo := NewPostgresRepository(db.DB)

	svc := NewService(repo, cheapHasher(), redis.Client, newCaptureMailer(), testSealer(t),
		NewCacheThrottle(redis.Client, redis.Client, nil), nil, nil, Options{})

	err := svc.RequireFundingAllowed(ctx, FundingRequest{
		PrincipalID: id.PrincipalID("00000000-0000-0000-0000-000000000000"),
		Amount:      usd(t, "1"),
	})
	assert.Error(t, err, "funding must refuse rather than assume an unbounded cap")
}
