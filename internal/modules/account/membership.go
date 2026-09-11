package account

import (
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// DefaultInvitationLifetime is how long an invitation link stays usable.
//
// A week, because an invite is often sent to somebody who is not expecting it
// and may not read their mail until Monday. It is longer than a password reset
// for that reason, and shorter than forever because an unclaimed invitation is
// a standing grant of access sitting in a mailbox.
const DefaultInvitationLifetime = 7 * 24 * time.Hour

// Invitation is an offer of membership in an account, addressed to an email.
type Invitation struct {
	ID        string
	AccountID id.AccountID

	// Email is who the offer was addressed to. Acceptance is checked against
	// it, so a forwarded link does not grant access to whoever opens it.
	Email string

	Role      PermissionRole
	InvitedBy id.UserID

	// AccountName is carried for the message only. A recipient who was not
	// expecting an invitation cannot tell an unexplained link asking for a
	// password from a phishing attempt, and naming the account is what makes
	// the difference.
	AccountName string

	ExpiresAt  time.Time
	AcceptedAt *time.Time
	RevokedAt  *time.Time
	CreatedAt  time.Time
}

// Errors reported by the membership flows.
var (
	// ErrInvitationInvalid covers unknown, expired, revoked and already
	// accepted alike, for the same reason ErrTokenInvalid does: a caller can
	// act no differently on one than another, and a specific answer would let
	// somebody probe which links were ever real.
	ErrInvitationInvalid = apperr.Unauthorized("this invitation is invalid or has expired")

	// ErrInvitationNotYours reports an invitation being accepted by somebody
	// other than the address it names.
	//
	// Invitations are forwarded all the time, deliberately and accidentally.
	// Without this the token alone would be the whole credential, and an
	// invite meant for a colleague would grant an account to whoever it
	// reached.
	ErrInvitationNotYours = apperr.Forbidden("this invitation was sent to a different address")

	// ErrAlreadyInvited reports a second outstanding invitation for the same
	// address. Resending is revoke-then-invite, so that the earlier link stops
	// working rather than both staying live.
	ErrAlreadyInvited = apperr.Conflict("that address already has an open invitation")

	// ErrAlreadyMember reports an invitation for somebody who is already in
	// the account. Safe to be specific about: the caller is a member of that
	// account and can already see who else is.
	ErrAlreadyMember = apperr.Conflict("that person is already a member of this account")

	// ErrCannotGrantOwner reports an attempt to invite or promote to owner.
	// There is exactly one owner, and the way it moves is transfer_owner,
	// which is delayed and can be vetoed.
	ErrCannotGrantOwner = apperr.Invalid("ownership cannot be granted, transfer it instead")

	// ErrNotPermitted reports an actor whose role does not cover this change.
	ErrNotPermitted = apperr.Forbidden("your role does not permit this change")

	// ErrGuardianRequiresControlChange reports an attempt to replace or remove
	// a guardian through membership.
	//
	// This is the most important refusal in this module. Every delay and veto
	// that protects an account assumes the guardian row exists, so if that row
	// could be deleted here, the entire safety net would be bypassable with a
	// write to a different table and none of the notifications would fire.
	ErrGuardianRequiresControlChange = apperr.Forbidden(
		"changing the guardian goes through an account control change")

	// ErrOwnerCannotLeave reports an attempt to remove or demote the owner.
	//
	// An account with no owner has nobody who can appoint one, so it is a state
	// with no way out that does not involve support.
	ErrOwnerCannotLeave = apperr.Forbidden(
		"the owner cannot leave an account, transfer ownership first")
)

// canManageRole reports whether an actor holding one role may create, remove or
// change a membership at another.
//
// The matrix, and why each line is what it is:
//
//	viewer    owner and admin. Read-only access is the one grant that cannot
//	          escalate, so delegating it costs nothing.
//	admin     owner only. Two admins who can each remove the other is a
//	          lockout race, and during a compromise the attacker wins it by
//	          acting first. Routing this through the owner keeps one
//	          accountable party for who is allowed in.
//	guardian  owner, and only to appoint the first one. Replacing or removing
//	          a guardian is handled elsewhere, under delay.
//	owner     nobody. Ownership moves by transfer, which is delayed and
//	          vetoable, never by an ordinary membership write.
func canManageRole(actor, target PermissionRole) error {
	switch target {
	case RoleOwner:
		return ErrCannotGrantOwner

	case RoleAdmin, RoleGuardian:
		if actor != RoleOwner {
			return ErrNotPermitted
		}
		return nil

	case RoleViewer:
		if actor != RoleOwner && actor != RoleAdmin {
			return ErrNotPermitted
		}
		return nil

	default:
		return errInvalid("unknown role")
	}
}

// raisesAuthority reports whether moving to a role grants more than viewing.
//
// Used to decide what needs a second factor. Granting authority is gated the
// way funding is, while removing it is not: taking access away is the safe
// direction, and an incident is exactly when somebody needs to revoke a
// colleague's access quickly and may not have their phone to hand.
func raisesAuthority(role PermissionRole) bool {
	return role == RoleAdmin || role == RoleGuardian
}
