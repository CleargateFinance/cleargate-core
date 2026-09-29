package account

import (
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// VerificationTier is what a principal is entitled to do, and it is the only
// thing that gates spending.
//
// Deliberately not gated on how money arrived. Tying verification to funding
// method produces the wrong shape: a twenty dollar card top-up would face more
// friction than twenty thousand in stablecoin, because the card route happens
// to touch a regulated intermediary and the other does not.
type VerificationTier string

const (
	// TierBasic has given an email address and nothing else. Low cap, and
	// funding only from screened addresses. This is the trial: somebody can
	// move real money through the system without ever handing over identity
	// documents, which is the point.
	TierBasic VerificationTier = "basic"

	// TierVerified has confirmed who they are. Standard limits.
	TierVerified VerificationTier = "verified"

	// TierBusiness has confirmed a legal entity and its beneficial ownership.
	// High limits.
	TierBusiness VerificationTier = "business"
)

// Valid reports whether t is a tier the system recognises.
func (t VerificationTier) Valid() bool {
	switch t {
	case TierBasic, TierVerified, TierBusiness:
		return true
	default:
		return false
	}
}

// Default tier limits, in USD.
//
// These are policy rather than physics, so they are constants a deployment can
// override rather than values baked into a migration. The basic cap is the
// number that matters: it is what makes an unverified account a bounded risk
// rather than an open one, and it should be low enough that the honest answer
// to "what if this is a laundering attempt" is "then they laundered ninety
// dollars through an address we screened and can name".
const (
	DefaultBasicFundingCap    = "100"
	DefaultVerifiedFundingCap = "10000"
	DefaultBusinessFundingCap = "250000"
)

// ScreeningStatus is what an address screening came back with.
//
// Screening is not verification and the two must never be conflated. It asks
// whether an address is associated with sanctioned or criminal activity, and
// says nothing at all about who controls it.
type ScreeningStatus string

const (
	// ScreeningPending has been submitted and not yet answered.
	ScreeningPending ScreeningStatus = "pending"

	// ScreeningClear may fund and receive.
	ScreeningClear ScreeningStatus = "clear"

	// ScreeningFlagged needs a human to look at it before anything moves.
	ScreeningFlagged ScreeningStatus = "flagged"

	// ScreeningRejected may not be used at all.
	ScreeningRejected ScreeningStatus = "rejected"
)

// ScreenedAddress is one blockchain address a principal funds from or
// withdraws to, and the verdict on it.
type ScreenedAddress struct {
	ID          string
	PrincipalID id.PrincipalID

	Chain   string
	Address string

	Status    ScreeningStatus
	Provider  string
	Reference string

	ScreenedAt *time.Time

	// FirstFundedAt is set by the first deposit that arrives from this address.
	// It is what makes return-to-source withdrawal possible: an unverified
	// principal may only withdraw back to an address that actually funded them.
	FirstFundedAt *time.Time

	CreatedAt time.Time
}

// Usable reports whether money may move through this address.
func (a ScreenedAddress) Usable() bool { return a.Status == ScreeningClear }

// HasFunded reports whether this address has ever sent money in, which is what
// return-to-source withdrawal checks against.
func (a ScreenedAddress) HasFunded() bool { return a.FirstFundedAt != nil }

// Errors reported by the verification and funding flows.
var (
	// ErrFundingCapExceeded reports funding that would take a principal past
	// what their tier allows. It is specific on purpose: the caller can act on
	// it by verifying, and telling them so is the whole conversion path.
	ErrFundingCapExceeded = apperr.Forbidden(
		"this would exceed the limit for your verification level")

	// ErrAddressNotScreened reports funding from an address that has no
	// verdict yet. Screening is asynchronous, so this is a wait rather than a
	// refusal, but it is never a bypass.
	ErrAddressNotScreened = apperr.Conflict("this address is still being checked")

	// ErrAddressRejected reports an address that may not be used. Deliberately
	// undivided from flagged: somebody probing which addresses a screening
	// provider dislikes is not a conversation worth having.
	ErrAddressRejected = apperr.Forbidden("this address cannot be used")

	// ErrWithdrawalNotToSource reports an unverified principal trying to
	// withdraw somewhere other than an address that funded them.
	//
	// This is the control that makes the unverified tier safe to offer at all.
	// Without it, "fund anonymously, withdraw elsewhere" is a laundering route,
	// and repeated across accounts it is textbook structuring. With it, an
	// unverified account cannot move value between two addresses, only return
	// what it brought.
	ErrWithdrawalNotToSource = apperr.Forbidden(
		"unverified accounts can only withdraw to an address they funded from")
)

// TierLimits is the funding ceiling for each tier.
type TierLimits struct {
	Basic    currency.Amount
	Verified currency.Amount
	Business currency.Amount
}

// CapFor returns the funding ceiling for a tier.
func (l TierLimits) CapFor(tier VerificationTier) currency.Amount {
	switch tier {
	case TierVerified:
		return l.Verified
	case TierBusiness:
		return l.Business
	default:
		return l.Basic
	}
}
