//go:build integration

package account

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// memberFixture is an account with an owner who holds a confirmed second
// factor, which is the starting point for everything that follows: inviting is
// step-up gated, so an owner who cannot satisfy it cannot invite anybody.
type memberFixture struct {
	*mfaFixture

	accountID id.AccountID
	owner     Session
}

func newMemberFixture(t *testing.T) *memberFixture {
	t.Helper()
	ctx := context.Background()

	mfa := newMFAFixture(t)
	secret, _ := mfa.enrol(t)

	var accountID string
	require.NoError(t, mfa.db.QueryRow(ctx,
		`SELECT account_id FROM account_membership WHERE user_id = $1`,
		string(mfa.user)).Scan(&accountID))

	token, _ := mfa.session(t)
	owner, err := mfa.svc.SatisfyTOTP(ctx, token, mfa.codeFor(t, secret), "203.0.113.1", "test")
	require.NoError(t, err)

	return &memberFixture{mfaFixture: mfa, accountID: id.AccountID(accountID), owner: owner}
}

// invite offers a role and returns the token the recipient would receive.
func (f *memberFixture) invite(t *testing.T, actor Session, email string, role PermissionRole) string {
	t.Helper()

	require.NoError(t, f.svc.InviteMember(context.Background(), InviteParams{
		Actor: actor, AccountID: f.accountID, Email: email, Role: role,
		IP: "203.0.113.1",
	}))

	token, ok := f.mailer.invitations[email]
	require.True(t, ok, "an invitation must send a link")
	return token
}

// joinAsNewUser accepts an invitation as somebody with no login yet.
func (f *memberFixture) joinAsNewUser(t *testing.T, token string) id.UserID {
	t.Helper()

	_, session, err := f.svc.AcceptInviteAsNewUser(
		context.Background(), token, goodPassword, "203.0.113.9", "test")
	require.NoError(t, err)
	return session.UserID
}

// signInStranger creates a login that belongs to no account here, and returns
// its session token. It stands in for somebody who is a perfectly legitimate
// user of the platform and has no business in this particular account.
func (f *memberFixture) signInStranger(t *testing.T, email string) string {
	t.Helper()
	ctx := context.Background()

	auth := &authFixture{db: f.db, cache: f.cache, svc: f.svc, repo: f.repo, mailer: f.mailer}
	userID := auth.signupAndVerify(t, email)

	token, _, err := f.svc.IssueSession(ctx, NewSessionParams{UserID: userID})
	require.NoError(t, err)
	return token.Plaintext()
}

// addMemberWithMFA brings somebody in at a role and returns a session of theirs
// that has satisfied a second factor, so they can act where step-up is required.
func (f *memberFixture) addMemberWithMFA(t *testing.T, email string, role PermissionRole) Session {
	t.Helper()
	ctx := context.Background()

	token := f.invite(t, f.owner, email, role)

	plaintext, session, err := f.svc.AcceptInviteAsNewUser(ctx, token, goodPassword, "203.0.113.9", "test")
	require.NoError(t, err)

	enrolment, err := f.svc.BeginTOTPEnrolment(ctx, session.UserID)
	require.NoError(t, err)

	_, err = f.svc.ConfirmTOTPEnrolment(ctx, session.UserID,
		f.codeFor(t, enrolment.Secret), "203.0.113.9", "test")
	require.NoError(t, err)

	// Confirming spends that step, so the next code has to come from the next
	// one, exactly as the replay guard intends.
	f.clock.Advance(time.Duration(crypto.TOTPPeriod) * time.Second)

	satisfied, err := f.svc.SatisfyTOTP(ctx, plaintext.Plaintext(),
		f.codeFor(t, enrolment.Secret), "203.0.113.9", "test")
	require.NoError(t, err)

	return satisfied
}

// memberRole reads a role straight from the database.
func (f *memberFixture) memberRole(t *testing.T, userID id.UserID) (string, bool) {
	t.Helper()

	var role string
	err := f.db.QueryRow(context.Background(),
		`SELECT role FROM account_membership WHERE account_id = $1 AND user_id = $2`,
		string(f.accountID), string(userID)).Scan(&role)
	if err != nil {
		return "", false
	}
	return role, true
}

func TestAcceptInvite_CreatesMembership(t *testing.T) {
	ctx := context.Background()
	f := newMemberFixture(t)

	token := f.invite(t, f.owner, "colleague@example.com", RoleViewer)

	// The message names the account, so a recipient who was not expecting it
	// can tell it from a phishing attempt.
	assert.Equal(t, "founder", f.mailer.invitationAccounts["colleague@example.com"])

	_, session, err := f.svc.AcceptInviteAsNewUser(ctx, token, goodPassword, "203.0.113.9", "test")
	require.NoError(t, err)

	role, ok := f.memberRole(t, session.UserID)
	require.True(t, ok, "accepting must create the membership")
	assert.Equal(t, string(RoleViewer), role)

	// No second account was created. They joined somebody else's.
	var accounts int
	require.NoError(t, f.db.QueryRow(ctx, `SELECT count(*) FROM account`).Scan(&accounts))
	assert.Equal(t, 1, accounts)

	// The address is already verified: opening the link proved it, so asking
	// again would be asking for the same proof twice.
	user, err := f.repo.UserByID(ctx, session.UserID)
	require.NoError(t, err)
	assert.True(t, user.EmailVerified())

	// And the session returned is usable, so the invitee is not asked to sign
	// in with the password they just chose.
	assert.NotEmpty(t, session.ID)
}

// TestInvite_CannotGrantOwner covers the role that has no invite path at all.
// There is exactly one owner, and it moves by transfer, which is delayed and
// can be vetoed.
func TestInvite_CannotGrantOwner(t *testing.T) {
	ctx := context.Background()
	f := newMemberFixture(t)

	err := f.svc.InviteMember(ctx, InviteParams{
		Actor: f.owner, AccountID: f.accountID,
		Email: "usurper@example.com", Role: RoleOwner,
	})
	assert.ErrorIs(t, err, ErrCannotGrantOwner)

	// Nothing was written, so a refused invite leaves no link in a mailbox.
	var invitations int
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT count(*) FROM account_invitation`).Scan(&invitations))
	assert.Equal(t, 0, invitations)
}

// TestInvite_TokenIsSingleUseAndExpires covers both ways a link stops working.
func TestInvite_TokenIsSingleUseAndExpires(t *testing.T) {
	ctx := context.Background()

	t.Run("single use", func(t *testing.T) {
		f := newMemberFixture(t)
		token := f.invite(t, f.owner, "once@example.com", RoleViewer)

		f.joinAsNewUser(t, token)

		// The same link again, by somebody who now has a login of their own.
		_, _, err := f.svc.AcceptInviteAsNewUser(ctx, token, goodPassword, "203.0.113.9", "test")
		assert.ErrorIs(t, err, ErrInvitationInvalid)
	})

	t.Run("expires", func(t *testing.T) {
		f := newMemberFixture(t)
		token := f.invite(t, f.owner, "late@example.com", RoleViewer)

		f.clock.Advance(DefaultInvitationLifetime + time.Minute)

		_, _, err := f.svc.AcceptInviteAsNewUser(ctx, token, goodPassword, "203.0.113.9", "test")
		assert.ErrorIs(t, err, ErrInvitationInvalid)
	})
}

func TestRevokedInviteCannotBeAccepted(t *testing.T) {
	ctx := context.Background()
	f := newMemberFixture(t)

	token := f.invite(t, f.owner, "recalled@example.com", RoleViewer)

	var invitationID string
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT id FROM account_invitation WHERE email = $1`,
		"recalled@example.com").Scan(&invitationID))

	require.NoError(t, f.svc.RevokeInvitation(ctx, f.owner, f.accountID, invitationID))

	_, _, err := f.svc.AcceptInviteAsNewUser(ctx, token, goodPassword, "203.0.113.9", "test")
	assert.ErrorIs(t, err, ErrInvitationInvalid)

	// Revoking twice is not an error worth a different answer, but there is
	// nothing left to revoke.
	assert.ErrorIs(t, f.svc.RevokeInvitation(ctx, f.owner, f.accountID, invitationID),
		ErrInvitationInvalid)
}

// TestInvite_CannotBeAcceptedByADifferentAddress is what stops a forwarded link
// from being the whole credential.
func TestInvite_CannotBeAcceptedByADifferentAddress(t *testing.T) {
	ctx := context.Background()
	f := newMemberFixture(t)

	intended := f.invite(t, f.owner, "intended@example.com", RoleViewer)

	// A user who exists but is not the invitee, as if the link were forwarded.
	strangerSession := f.signInStranger(t, "stranger@example.com")

	err := f.svc.AcceptInviteAsExistingUser(ctx, intended, strangerSession, "203.0.113.9", "test")
	assert.ErrorIs(t, err, ErrInvitationNotYours)

	// And crucially the invitation is not burned, so the person it was meant
	// for can still use it.
	_, session, err := f.svc.AcceptInviteAsNewUser(ctx, intended, goodPassword, "203.0.113.9", "test")
	require.NoError(t, err)

	role, ok := f.memberRole(t, session.UserID)
	require.True(t, ok)
	assert.Equal(t, string(RoleViewer), role)
}

// TestAdminCannotManageOtherAdmins keeps the owner as the single accountable
// party for who is allowed in.
//
// Two admins able to remove each other is a lockout race, and during a
// compromise the attacker wins it by acting first.
func TestAdminCannotManageOtherAdmins(t *testing.T) {
	ctx := context.Background()
	f := newMemberFixture(t)

	adminSession := f.addMemberWithMFA(t, "admin@example.com", RoleAdmin)
	otherAdmin := f.joinAsNewUser(t, f.invite(t, f.owner, "other-admin@example.com", RoleAdmin))

	// An admin may not invite another admin.
	err := f.svc.InviteMember(ctx, InviteParams{
		Actor: adminSession, AccountID: f.accountID,
		Email: "third-admin@example.com", Role: RoleAdmin,
	})
	assert.ErrorIs(t, err, ErrNotPermitted)

	// Nor remove one.
	assert.ErrorIs(t, f.svc.RemoveMember(ctx, adminSession, f.accountID, otherAdmin),
		ErrNotPermitted)

	// Nor demote one.
	assert.ErrorIs(t,
		f.svc.ChangeMemberRole(ctx, adminSession, f.accountID, otherAdmin, RoleViewer),
		ErrNotPermitted)

	// The other admin is still there after all three attempts.
	role, ok := f.memberRole(t, otherAdmin)
	require.True(t, ok)
	assert.Equal(t, string(RoleAdmin), role)

	// An admin may still invite a viewer, which is the one grant that cannot
	// escalate.
	assert.NoError(t, f.svc.InviteMember(ctx, InviteParams{
		Actor: adminSession, AccountID: f.accountID,
		Email: "reader@example.com", Role: RoleViewer,
	}))
}

// TestGuardianCannotBeRemovedByMembershipDelete is the most important refusal
// in this module.
//
// Every delay and veto protecting an account assumes the guardian row exists.
// If it could be deleted through an ordinary membership write, the whole safety
// net would be bypassable by writing to a different table, and none of the
// notifications or delays would fire.
func TestGuardianCannotBeRemovedByMembershipDelete(t *testing.T) {
	ctx := context.Background()
	f := newMemberFixture(t)

	guardian := f.joinAsNewUser(t, f.invite(t, f.owner, "guardian@example.com", RoleGuardian))

	// Not by the owner, who is the most privileged role there is.
	assert.ErrorIs(t, f.svc.RemoveMember(ctx, f.owner, f.accountID, guardian),
		ErrGuardianRequiresControlChange)

	// Not by demoting them out of the role either, which would be the same
	// removal wearing a different name.
	assert.ErrorIs(t,
		f.svc.ChangeMemberRole(ctx, f.owner, f.accountID, guardian, RoleViewer),
		ErrGuardianRequiresControlChange)

	// Nor by promoting somebody else into the role over the top of them.
	viewer := f.joinAsNewUser(t, f.invite(t, f.owner, "hopeful@example.com", RoleViewer))
	assert.ErrorIs(t,
		f.svc.ChangeMemberRole(ctx, f.owner, f.accountID, viewer, RoleGuardian),
		ErrGuardianRequiresControlChange)

	role, ok := f.memberRole(t, guardian)
	require.True(t, ok, "the guardian must still be a member")
	assert.Equal(t, string(RoleGuardian), role)
}

// TestFirstGuardianInviteIsInstant separates appointing a guardian from
// replacing one.
//
// The first appointment adds a control and needs no delay: there is nothing
// yet to protect it from. Replacing that guardian is where the delay belongs,
// because an owner whose session was stolen could otherwise swap out the one
// party able to stop them.
func TestFirstGuardianInviteIsInstant(t *testing.T) {
	ctx := context.Background()
	f := newMemberFixture(t)

	guardian := f.joinAsNewUser(t, f.invite(t, f.owner, "first@example.com", RoleGuardian))

	role, ok := f.memberRole(t, guardian)
	require.True(t, ok)
	assert.Equal(t, string(RoleGuardian), role, "the first guardian takes effect immediately")

	// A second one is not an appointment, it is a replacement.
	err := f.svc.InviteMember(ctx, InviteParams{
		Actor: f.owner, AccountID: f.accountID,
		Email: "second@example.com", Role: RoleGuardian,
	})
	assert.ErrorIs(t, err, ErrGuardianRequiresControlChange)
}

// TestLastOwnerCannotLeaveAccount covers the state with no way out.
//
// An account with no owner has nobody who can appoint one, so it takes support
// to repair. Leaving and being removed are the same operation, so both are
// refused by the same check.
func TestLastOwnerCannotLeaveAccount(t *testing.T) {
	ctx := context.Background()
	f := newMemberFixture(t)

	// The owner removing themselves.
	assert.ErrorIs(t, f.svc.RemoveMember(ctx, f.owner, f.accountID, f.user),
		ErrOwnerCannotLeave)

	// And demoting themselves, which would leave the account ownerless just as
	// effectively.
	assert.ErrorIs(t,
		f.svc.ChangeMemberRole(ctx, f.owner, f.accountID, f.user, RoleViewer),
		ErrOwnerCannotLeave)

	role, ok := f.memberRole(t, f.user)
	require.True(t, ok)
	assert.Equal(t, string(RoleOwner), role)
}

// TestInvite_RequiresStepUp is the gate built in the previous task doing its
// job on its first real caller.
func TestInvite_RequiresStepUp(t *testing.T) {
	ctx := context.Background()
	f := newMemberFixture(t)

	// Walk past the window. The factor is still enrolled, so the console must
	// be told to ask for a code rather than to start an enrolment.
	f.clock.Advance(DefaultStepUpWindow + time.Minute)

	err := f.svc.InviteMember(ctx, InviteParams{
		Actor: f.owner, AccountID: f.accountID,
		Email: "unreachable@example.com", Role: RoleViewer,
	})

	var stepUp *StepUpError
	require.ErrorAs(t, err, &stepUp)
	assert.Equal(t, ReasonStepUpRequired, stepUp.Reason)

	var invitations int
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT count(*) FROM account_invitation`).Scan(&invitations))
	assert.Equal(t, 0, invitations, "a refused invite must not send a link")
}

// TestRemoveMember_NeedsNoStepUp is the deliberate asymmetry.
//
// Taking access away is the safe direction, and the moment it matters most is
// a compromise, when the person who needs to act may not have their phone.
func TestRemoveMember_NeedsNoStepUp(t *testing.T) {
	ctx := context.Background()
	f := newMemberFixture(t)

	viewer := f.joinAsNewUser(t, f.invite(t, f.owner, "leaving@example.com", RoleViewer))

	f.clock.Advance(DefaultStepUpWindow + time.Minute)

	require.NoError(t, f.svc.RemoveMember(ctx, f.owner, f.accountID, viewer))

	_, stillMember := f.memberRole(t, viewer)
	assert.False(t, stillMember)
}

// TestInvite_RefusesSomebodyAlreadyInside covers both a current member and a
// link already sitting in their inbox.
func TestInvite_RefusesSomebodyAlreadyInside(t *testing.T) {
	ctx := context.Background()
	f := newMemberFixture(t)

	f.invite(t, f.owner, "twice@example.com", RoleViewer)

	// A second outstanding invitation would leave two live links for one
	// address, and revoking one would not stop the other.
	err := f.svc.InviteMember(ctx, InviteParams{
		Actor: f.owner, AccountID: f.accountID,
		Email: "twice@example.com", Role: RoleViewer,
	})
	assert.ErrorIs(t, err, ErrAlreadyInvited)

	// Once they are in, the answer changes.
	f.joinAsNewUser(t, f.mailer.invitations["twice@example.com"])

	err = f.svc.InviteMember(ctx, InviteParams{
		Actor: f.owner, AccountID: f.accountID,
		Email: "twice@example.com", Role: RoleAdmin,
	})
	assert.ErrorIs(t, err, ErrAlreadyMember)
}

// TestNonMemberCannotInvite keeps somebody with a perfectly good session for
// their own account out of everybody else's.
func TestNonMemberCannotInvite(t *testing.T) {
	ctx := context.Background()
	f := newMemberFixture(t)

	stranger := f.signInStranger(t, "outsider@example.com")
	session, err := f.svc.ValidateSession(ctx, stranger)
	require.NoError(t, err)

	err = f.svc.InviteMember(ctx, InviteParams{
		Actor: session, AccountID: f.accountID,
		Email: "friend@example.com", Role: RoleViewer,
	})
	assert.ErrorIs(t, err, ErrNotPermitted)
}
