package account

import (
	"context"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// RemoveMember revokes somebody's access to an account.
//
// Two refusals here are not policy preferences, they are the difference between
// a safety net that holds and one that can be written around:
//
//   - the guardian cannot be removed through this path at all, because every
//     delay and veto in the account assumes that row exists;
//   - the owner cannot be removed, because an account with no owner has nobody
//     who can appoint one.
//
// Deliberately not gated on a second factor. Removal only ever takes access
// away, and the moment it is most needed is a compromise, when the person
// holding the phone may be the one being removed.
func (s *Service) RemoveMember(ctx context.Context, actor Session, accountID id.AccountID, target id.UserID) error {
	actorRole, err := s.repo.MembershipRole(ctx, accountID, actor.UserID)
	if err != nil {
		return err
	}

	targetRole, err := s.repo.MembershipRole(ctx, accountID, target)
	if err != nil {
		return err
	}

	if targetRole == RoleGuardian {
		return ErrGuardianRequiresControlChange
	}
	if targetRole == RoleOwner {
		// Covers the owner removing themselves as well as anybody else trying.
		// Leaving is the same operation as being removed, and an account whose
		// only owner has walked out cannot appoint a new one.
		return ErrOwnerCannotLeave
	}

	if err := canManageRole(actorRole, targetRole); err != nil {
		return err
	}

	removed, err := s.repo.RemoveMembership(ctx, accountID, target)
	if err != nil {
		return err
	}
	if !removed {
		return ErrNotPermitted
	}

	// Their sessions are deliberately left alone. Authority in an account comes
	// from the membership row, which is read on every action and is now gone,
	// so removal takes effect on their next request without touching anything
	// else. Revoking their sessions would sign them out of every other account
	// they belong to, which is not what being removed from this one means.
	return nil
}

// ChangeMemberRole moves an existing member between roles.
//
// Both the role being left and the role being taken are checked against the
// actor. Checking only the destination would let an admin demote an owner to
// viewer, since viewer is a role admins may otherwise grant.
func (s *Service) ChangeMemberRole(ctx context.Context, actor Session, accountID id.AccountID, target id.UserID, role PermissionRole) error {
	if !role.Valid() {
		return errInvalid("unknown role")
	}

	actorRole, err := s.repo.MembershipRole(ctx, accountID, actor.UserID)
	if err != nil {
		return err
	}

	currentRole, err := s.repo.MembershipRole(ctx, accountID, target)
	if err != nil {
		return err
	}

	if currentRole == RoleGuardian || role == RoleGuardian {
		// Appointing a guardian is an invitation, and changing one is a control
		// change. Neither is a role edit.
		return ErrGuardianRequiresControlChange
	}
	if currentRole == RoleOwner {
		return ErrOwnerCannotLeave
	}
	if role == RoleOwner {
		return ErrCannotGrantOwner
	}

	if err := canManageRole(actorRole, currentRole); err != nil {
		return err
	}
	if err := canManageRole(actorRole, role); err != nil {
		return err
	}

	// Only the direction that hands out more authority needs a second factor.
	// Demoting somebody is as safe as removing them, and gating it would put a
	// phone between an owner and containing an incident.
	if raisesAuthority(role) {
		if err := s.RequireStepUp(ctx, actor); err != nil {
			return err
		}
	}

	if role == currentRole {
		return nil
	}

	changed, err := s.repo.UpdateMembershipRole(ctx, accountID, target, role)
	if err != nil {
		return err
	}
	if !changed {
		return ErrNotPermitted
	}

	return nil
}
