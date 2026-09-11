package account

import (
	"context"
	"log/slog"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// InviteParams is what a member submits to offer somebody a role.
type InviteParams struct {
	// Actor is the session of the person doing the inviting. A session rather
	// than a user id, because inviting is gated on a recent second factor and
	// only a session carries when that was satisfied.
	Actor Session

	AccountID id.AccountID
	Email     string
	Role      PermissionRole

	IP        string
	UserAgent string
}

// InviteMember offers somebody a role in an account and emails them a link.
//
// The checks run in order of cost, and nothing is written until every one has
// passed: the actor's role, whether this grant is permitted at all, the
// guardian rule, then the second factor.
func (s *Service) InviteMember(ctx context.Context, p InviteParams) error {
	email := NormalizeEmail(p.Email)
	if err := ValidateEmail(email); err != nil {
		return err
	}
	if !p.Role.Valid() {
		return errInvalid("unknown role")
	}

	actorRole, err := s.repo.MembershipRole(ctx, p.AccountID, p.Actor.UserID)
	if err != nil {
		return err
	}
	if err := canManageRole(actorRole, p.Role); err != nil {
		return err
	}

	// Appointing the first guardian is an ordinary invitation, because it adds
	// a control rather than altering one. Replacing the guardian that results
	// is not, and goes through a delayed, vetoable change instead: otherwise an
	// owner whose session was stolen could swap out the one party able to stop
	// them, and do it instantly.
	if p.Role == RoleGuardian {
		hasGuardian, err := s.repo.AccountHasGuardian(ctx, p.AccountID)
		if err != nil {
			return err
		}
		if hasGuardian {
			return ErrGuardianRequiresControlChange
		}
	}

	// Adding a person is gated the way adding money is. A stolen session cookie
	// alone must not be enough to put somebody new inside an account, because
	// the grant it creates outlives the session that created it.
	if err := s.RequireStepUp(ctx, p.Actor); err != nil {
		return err
	}

	member, err := s.repo.IsMember(ctx, p.AccountID, email)
	if err != nil {
		return err
	}
	if member {
		return ErrAlreadyMember
	}

	token, err := crypto.NewToken()
	if err != nil {
		return err
	}

	invitation, err := s.repo.CreateInvitation(ctx, Invitation{
		AccountID: p.AccountID,
		Email:     email,
		Role:      p.Role,
		InvitedBy: p.Actor.UserID,
		ExpiresAt: s.clock.Now().Add(s.invitationLifetime),
	}, token.Digest())
	if err != nil {
		return err
	}

	s.log.Info("invitation created",
		slog.String("account_id", string(p.AccountID)),
		slog.String("role", string(p.Role)),
		slog.String("invitation_id", invitation.ID),
	)

	if err := s.mailer.SendInvitation(ctx, email, token.Plaintext(), invitation.AccountName); err != nil {
		// The invitation exists and can be resent, so this deserves an alert
		// rather than an error suggesting nothing happened.
		s.log.Error("sending invitation failed", slog.String("error", err.Error()))
	}

	return nil
}

// RevokeInvitation withdraws an outstanding offer.
//
// Not gated on a second factor. Withdrawing only ever removes a grant, and the
// moment somebody most needs to do it is the moment they have realised the
// link went to the wrong address.
func (s *Service) RevokeInvitation(ctx context.Context, actor Session, accountID id.AccountID, invitationID string) error {
	actorRole, err := s.repo.MembershipRole(ctx, accountID, actor.UserID)
	if err != nil {
		return err
	}
	if actorRole != RoleOwner && actorRole != RoleAdmin {
		return ErrNotPermitted
	}

	revoked, err := s.repo.RevokeInvitation(ctx, invitationID, accountID, s.clock.Now())
	if err != nil {
		return err
	}
	if !revoked {
		return ErrInvitationInvalid
	}

	return nil
}

// AcceptInviteAsNewUser accepts an invitation for somebody who has no login
// yet, creating one and granting the membership together.
//
// A login with no membership leaves the person staring at an empty console
// with no route to the account they were invited to, and a membership with no
// login is a row pointing at nobody. The repository writes both or neither.
func (s *Service) AcceptInviteAsNewUser(ctx context.Context, plaintext, password, ip, userAgent string) (crypto.Token, Session, error) {
	if plaintext == "" {
		return crypto.Token{}, Session{}, ErrInvitationInvalid
	}
	if err := ValidatePassword(password); err != nil {
		return crypto.Token{}, Session{}, err
	}

	digest := crypto.DigestToken(plaintext)

	// Read first, because the address this creates a login for comes from the
	// invitation rather than from the caller. The claim below is still what
	// decides whether acceptance proceeds, so two people opening the same link
	// cannot both get through here.
	invitation, err := s.repo.InvitationByDigest(ctx, digest)
	if err != nil {
		return crypto.Token{}, Session{}, err
	}

	hash, err := s.hasher.Hash(password)
	if err != nil {
		return crypto.Token{}, Session{}, err
	}

	now := s.clock.Now()

	user, invitation, err := s.repo.AcceptInvitationAsNewUser(ctx, digest, CreateUserParams{
		Email:             invitation.Email,
		PasswordHash:      hash,
		DisplayName:       defaultDisplayName(invitation.Email),
		PasswordChangedAt: now,
	}, now)
	if err != nil {
		return crypto.Token{}, Session{}, err
	}

	s.audit(ctx, AuthEvent{
		UserID: user.ID, Email: invitation.Email, Type: EventInviteAccepted,
		IP: ip, UserAgent: userAgent,
	})

	// Signed in straight away. They have just proved control of the address and
	// chosen a password, so asking them to type it again proves nothing.
	return s.IssueSession(ctx, NewSessionParams{
		UserID: user.ID, IP: ip, UserAgent: userAgent,
	})
}

// AcceptInviteAsExistingUser accepts an invitation on behalf of somebody who
// already has a login.
//
// The session's address must match the one the invitation names. Invitations
// are forwarded all the time, deliberately and by accident, and without this
// the token alone would be the entire credential: an invite meant for a
// colleague would let whoever received it into an account holding money.
func (s *Service) AcceptInviteAsExistingUser(ctx context.Context, plaintext, sessionToken, ip, userAgent string) error {
	if plaintext == "" {
		return ErrInvitationInvalid
	}

	session, err := s.ValidateSession(ctx, sessionToken)
	if err != nil {
		return err
	}

	user, err := s.repo.UserByID(ctx, session.UserID)
	if err != nil {
		return err
	}

	digest := crypto.DigestToken(plaintext)

	invitation, err := s.repo.InvitationByDigest(ctx, digest)
	if err != nil {
		return err
	}

	// Checked before claiming, so a link opened by the wrong person is not
	// burned on their behalf and the intended recipient can still use it.
	// Being specific here costs nothing: whoever holds this token received the
	// message that carried it and already knows who it names.
	if user.Email != invitation.Email {
		return ErrInvitationNotYours
	}

	if _, err := s.repo.AcceptInvitationAsMember(ctx, digest, user.ID, s.clock.Now()); err != nil {
		return err
	}

	s.audit(ctx, AuthEvent{
		UserID: user.ID, Email: user.Email, Type: EventInviteAccepted,
		IP: ip, UserAgent: userAgent,
	})

	return nil
}
