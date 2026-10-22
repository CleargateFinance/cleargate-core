package account

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// FundingRequest is money about to arrive.
type FundingRequest struct {
	PrincipalID id.PrincipalID
	Amount      currency.Amount

	// Chain and Address identify where the money is coming from. Both are
	// empty for a fiat on-ramp deposit, where there is no counterparty address
	// to screen and the on-ramp has already done customer due diligence of its
	// own. Chain is a CAIP-2 network id, e.g. eip155:8453.
	Chain   string
	Address string
}

// FromChain reports whether this deposit arrives from an address we can screen.
func (r FundingRequest) FromChain() bool {
	return r.Chain != "" && r.Address != ""
}

// RequireFundingAllowed reports whether money may be accepted.
//
// Two independent questions, and both must pass. Does the tier permit this
// much in total, and — for money arriving from a blockchain address — is that
// address clear. They are separate because they fail for separate reasons and
// a caller acts differently on each: a cap is fixed by verifying, a screening
// verdict is not fixed by anything the customer can do.
//
// This is the gate every funding path goes through. It lives here rather than
// in a webhook handler because the webhook is one path to a deposit today, and
// reconciliation repair, manual credits and a second provider are the paths
// that arrive later. Putting the check where funding is decided means every
// future caller inherits it instead of being asked to remember.
func (s *Service) RequireFundingAllowed(ctx context.Context, r FundingRequest) error {
	if r.Amount.IsNegative() || r.Amount.IsZero() {
		return errInvalid("funding amount must be positive")
	}

	if r.FromChain() {
		if err := s.requireAddressUsable(ctx, r.PrincipalID, r.Chain, r.Address); err != nil {
			return err
		}
	}

	if s.funding == nil {
		// Refusing is the only safe answer. Assuming zero funded-to-date would
		// turn a missing dependency into an unbounded cap, silently, on the one
		// path where that matters most.
		return apperr.Internal("account: funding history is not wired", nil)
	}

	tier, err := s.repo.VerificationTier(ctx, r.PrincipalID)
	if err != nil {
		return err
	}

	funded, err := s.funding.FundedToDate(ctx, r.PrincipalID)
	if err != nil {
		return err
	}

	total, err := funded.Add(r.Amount)
	if err != nil {
		return err
	}

	ceiling := s.tierLimits.CapFor(tier)
	over, err := total.GreaterThan(ceiling)
	if err != nil {
		return err
	}
	if over {
		s.log.Info("funding refused, tier cap reached",
			slog.String("principal_id", string(r.PrincipalID)),
			slog.String("tier", string(tier)),
		)
		return ErrFundingCapExceeded
	}

	return nil
}

// RecordFunding stamps the source address as having funded this principal.
//
// Called after a deposit actually lands, not before. The stamp is what
// return-to-source withdrawal is checked against, so writing it on an intent
// rather than on an arrival would let somebody unlock a withdrawal destination
// by starting a deposit they never completed.
func (s *Service) RecordFunding(ctx context.Context, r FundingRequest, at time.Time) error {
	if !r.FromChain() {
		return nil
	}
	address, err := normalizeAddress(r.Chain, r.Address)
	if err != nil {
		return err
	}
	return s.repo.MarkAddressFunded(ctx, r.PrincipalID, r.Chain, address, at)
}

// WithdrawalRequest is money about to leave.
type WithdrawalRequest struct {
	PrincipalID id.PrincipalID
	Amount      currency.Amount

	Chain   string
	Address string
}

// RequireWithdrawalAllowed reports whether money may leave to this address.
//
// An unverified principal may only withdraw back to an address that actually
// funded them. This single rule is what makes the unverified tier safe to offer
// at all: without it, funding anonymously and withdrawing elsewhere is a
// laundering route, and repeated across accounts it is textbook structuring.
// With it, an unverified account cannot move value between two addresses — it
// can only return what it brought.
//
// Verification lifts the restriction, which is the honest trade being offered:
// tell us who you are and you may send money where you like.
func (s *Service) RequireWithdrawalAllowed(ctx context.Context, r WithdrawalRequest) error {
	if r.Amount.IsNegative() || r.Amount.IsZero() {
		return errInvalid("withdrawal amount must be positive")
	}
	if r.Chain == "" || r.Address == "" {
		return errInvalid("withdrawal needs a destination")
	}

	address, err := normalizeAddress(r.Chain, r.Address)
	if err != nil {
		return err
	}

	// The destination is screened regardless of tier. A verified principal is
	// still not permitted to send money to a sanctioned address, and
	// verification was never a claim about where they send it.
	if err := s.requireAddressUsable(ctx, r.PrincipalID, r.Chain, address); err != nil {
		return err
	}

	tier, err := s.repo.VerificationTier(ctx, r.PrincipalID)
	if err != nil {
		return err
	}
	if tier != TierBasic {
		return nil
	}

	sources, err := s.repo.FundingAddresses(ctx, r.PrincipalID)
	if err != nil {
		return err
	}
	for _, source := range sources {
		if source.Chain == r.Chain && source.Address == address && source.HasFunded() {
			return nil
		}
	}

	return ErrWithdrawalNotToSource
}

// RecordScreening stores a screening verdict. It is the only write path for
// screened addresses, so every stored address is normalized.
func (s *Service) RecordScreening(ctx context.Context, a ScreenedAddress) (ScreenedAddress, error) {
	address, err := normalizeAddress(a.Chain, a.Address)
	if err != nil {
		return ScreenedAddress{}, err
	}
	a.Address = address
	return s.repo.RecordScreening(ctx, a)
}

// requireAddressUsable refuses an address that has no verdict, or a bad one.
func (s *Service) requireAddressUsable(ctx context.Context, principalID id.PrincipalID, chain, address string) error {
	address, err := normalizeAddress(chain, address)
	if err != nil {
		return err
	}

	screened, err := s.repo.ScreenedAddress(ctx, principalID, chain, address)
	if err != nil {
		if errors.Is(err, ErrAddressNotScreened) {
			// Screening is asynchronous, so no verdict yet is a wait rather
			// than a refusal. It is never a bypass: money does not move on an
			// address nobody has looked at.
			return ErrAddressNotScreened
		}
		return err
	}

	if !screened.Usable() {
		// Flagged and rejected answer identically. Somebody probing which
		// addresses a screening provider dislikes is not a conversation worth
		// having, and the operator sees the real verdict in the record.
		s.log.Warn("address refused by screening",
			slog.String("principal_id", string(principalID)),
			slog.String("status", string(screened.Status)),
		)
		return ErrAddressRejected
	}

	return nil
}

// normalizeAddress puts an address into the form it is stored and compared in.
// chain is a CAIP-2 network id. EVM addresses are lowercased, since mixed case
// is only a checksum. Solana addresses are case-sensitive and kept as given.
func normalizeAddress(chain, address string) (string, error) {
	address = strings.TrimSpace(address)

	namespace, reference, ok := strings.Cut(chain, ":")
	if !ok || reference == "" {
		return "", errInvalid("chain must be a CAIP-2 network id")
	}

	switch namespace {
	case "eip155":
		return strings.ToLower(address), nil
	case "solana":
		return address, nil
	default:
		return "", errInvalid("unsupported chain " + chain)
	}
}
