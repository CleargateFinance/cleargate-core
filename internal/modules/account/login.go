package account

import (
	"context"
	"errors"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
)

// LoginParams is what a person submits to sign in.
type LoginParams struct {
	Email     string
	Password  string
	IP        string
	UserAgent string
}

// Login exchanges an address and password for a session token.
//
// The order of the checks is the security design, not an accident:
//
//  1. the throttle, so a guessing run is slowed before it costs a hash;
//  2. the password, always, even for an address that does not exist;
//  3. only then whether the address is verified and the login usable.
//
// Step three has to come last. Checking a user's status before their password
// would answer "this account is suspended" to anyone who typed the address,
// which tells an attacker the address is real. After a correct password there
// is nothing left to leak, so those answers can be specific and useful.
func (s *Service) Login(ctx context.Context, p LoginParams) (crypto.Token, Session, error) {
	email := NormalizeEmail(p.Email)
	key := ThrottleKey("login", p.IP, email)

	if wait, err := s.throttle.RetryAfter(ctx, key); err == nil && wait > 0 {
		return crypto.Token{}, Session{}, newThrottled(wait)
	}

	user, err := s.repo.UserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, ErrCredentialsInvalid) {
			// No such address. Spend what a real verification spends before
			// answering, or the speed of this reply becomes a reliable way to
			// enumerate which addresses are registered.
			s.hasher.DummyVerify()
			s.failLogin(ctx, key, AuthEvent{
				Email: email, Type: EventLoginFailed,
				IP: p.IP, UserAgent: p.UserAgent,
			})
			return crypto.Token{}, Session{}, ErrCredentialsInvalid
		}
		return crypto.Token{}, Session{}, err
	}

	ok, err := s.hasher.Verify(user.PasswordHash, p.Password)
	if err != nil {
		return crypto.Token{}, Session{}, err
	}
	if !ok {
		s.failLogin(ctx, key, AuthEvent{
			UserID: user.ID, Email: email, Type: EventLoginFailed,
			IP: p.IP, UserAgent: p.UserAgent,
		})
		return crypto.Token{}, Session{}, ErrCredentialsInvalid
	}

	// Past this point the caller has proven they hold the password, so a
	// specific reason is safe to give and far more useful than a generic one.
	if !user.EmailVerified() {
		return crypto.Token{}, Session{}, ErrEmailNotVerified
	}
	if user.Status != UserActive {
		return crypto.Token{}, Session{}, ErrUserSuspended
	}

	// A correct password clears the record, so someone who mistyped a few
	// times and then got it right is not still serving a penalty.
	if err := s.throttle.Succeed(ctx, key); err != nil {
		s.log.Warn("clearing login throttle failed")
	}

	// The plaintext is in hand exactly here and nowhere else in the system, so
	// this is the only moment a hash written under weaker settings can be
	// upgraded. Its failure is not the person's problem.
	s.upgradePasswordHash(ctx, user, p.Password)

	token, session, err := s.IssueSession(ctx, NewSessionParams{
		UserID: user.ID,
		// A second factor has not been presented. Enrolment and the step-up
		// window arrive with MFA, and until then no session claims to have
		// satisfied one.
		MFASatisfied: false,
		IP:           p.IP,
		UserAgent:    p.UserAgent,
	})
	if err != nil {
		return crypto.Token{}, Session{}, err
	}

	s.audit(ctx, AuthEvent{
		UserID: user.ID, Email: email, Type: EventLoginSucceeded,
		IP: p.IP, UserAgent: p.UserAgent,
	})

	return token, session, nil
}

// Logout ends the session behind a token.
func (s *Service) Logout(ctx context.Context, plaintext, ip, userAgent string) error {
	// Read the session before revoking it, so the audit entry can name whose
	// it was. A token that resolves to nothing is still a successful logout
	// from the client's point of view.
	session, err := s.ValidateSession(ctx, plaintext)
	if err != nil {
		return nil
	}

	if err := s.RevokeSession(ctx, plaintext); err != nil {
		return err
	}

	s.audit(ctx, AuthEvent{
		UserID: session.UserID, Type: EventSessionRevoked,
		IP: ip, UserAgent: userAgent,
	})

	return nil
}

// failLogin records an unsuccessful attempt against both the audit trail and
// the throttle.
func (s *Service) failLogin(ctx context.Context, key string, e AuthEvent) {
	if _, err := s.throttle.Fail(ctx, key); err != nil {
		s.log.Warn("recording login failure in the throttle failed")
	}
	s.audit(ctx, e)
}

// upgradePasswordHash rewrites a stored hash that was produced under weaker
// settings than are now current.
//
// This is the whole migration strategy for raising cost: no batch job, no
// downtime, and no rows that cannot be read. Every failure is swallowed,
// because the person signing in has done nothing wrong and their hash being a
// generation behind is not their problem.
func (s *Service) upgradePasswordHash(ctx context.Context, user User, password string) {
	stale, err := s.hasher.NeedsRehash(user.PasswordHash)
	if err != nil || !stale {
		return
	}

	hash, err := s.hasher.Hash(password)
	if err != nil {
		s.log.Warn("rehashing password failed")
		return
	}

	// password_changed_at deliberately keeps its existing value: the password
	// itself did not change, only how it is stored, and moving that timestamp
	// would sign the person out of every other device for no reason.
	if err := s.repo.UpdatePasswordHashOnly(ctx, user.ID, hash); err != nil {
		s.log.Warn("storing upgraded password hash failed")
	}
}
