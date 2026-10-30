package account

import (
	"context"
)

// RequireStepUp reports whether a session may perform an action that adds money
// or adds a person.
//
// Two conditions, checked in this order, and both belong here rather than at
// each call site. Scattering them would mean every future caller re-derives the
// policy, and the one that forgets a condition is a hole nobody notices.
//
//  1. a confirmed second factor exists at all, and
//  2. it was presented recently enough to still count.
//
// The reason codes are distinct because the console must do different things:
// one opens an enrolment wizard, the other asks for six digits.
//
// What this deliberately does not cover: signing a mandate, adding a payout
// destination, and proposing a control change. Those redirect money rather than
// merely adding it, and the window here is backed by TOTP, which is phishable —
// an attacker who relays one code through a convincing proxy holds a valid
// step-up for the next quarter of an hour. Those actions carry their own
// signature over the exact payload instead, which proves intent about that
// specific thing rather than presence at some point in the recent past.
//
// The MVP makes one deliberate exception: until passkey signing ships
// (Task 3.1), a mandate is approved by this step-up, through
// RequireMandateIssuer. The weakness above is accepted for the testnet MVP,
// not forgotten.
func (s *Service) RequireStepUp(ctx context.Context, session Session) error {
	enrolled, err := s.repo.HasConfirmedMFA(ctx, session.UserID)
	if err != nil {
		// Failing closed is right here and wrong for the throttle, and the
		// difference is what each protects. A throttle that fails closed turns a
		// cache blip into nobody being able to sign in. This gate failing open
		// would let a database blip authorise funding.
		return err
	}
	if !enrolled {
		return newStepUpError(ReasonMFARequired,
			"a second factor must be enrolled before this action")
	}

	if !session.MFARecent(s.clock.Now(), s.stepUpWindow) {
		return newStepUpError(ReasonStepUpRequired,
			"this action needs your second factor re-entered")
	}

	return nil
}

// MFAEnrolled reports whether a user holds a usable second factor, for the
// console to show enrolment state without attempting a gated action first.
func (s *Service) MFAEnrolled(ctx context.Context, session Session) (bool, error) {
	return s.repo.HasConfirmedMFA(ctx, session.UserID)
}
