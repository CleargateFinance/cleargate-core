package account

import (
	"context"
	"errors"
	"log/slog"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Documents a user consents to at sign-up.
const (
	DocumentTermsOfService = "terms_of_service"
	DocumentPrivacyPolicy  = "privacy_policy"
)

// SignupParams is what a person submits to create a login.
type SignupParams struct {
	Email    string
	Password string

	// DisplayName names the account being created. Empty is fine: the local
	// part of the address is used, and the owner can rename it later.
	DisplayName string

	IP        string
	UserAgent string
}

// Signup creates a principal, a login, an account, and the owner membership
// that ties them together, then emails a verification link.
//
// It reports success whether or not the address was already registered, and
// this is the single most important property of the function. Sign-up is one
// of three endpoints that would otherwise let someone take a leaked list of
// addresses and learn which of them belong to people holding balances here,
// which is precisely the list a phisher wants. So both paths do the same work,
// take about the same time, and answer the same way. Someone who was already
// registered gets no new account and no verification link; they get nothing at
// all, and the person probing learns nothing.
//
// A caller must therefore not treat a nil return as "the account now exists".
func (s *Service) Signup(ctx context.Context, p SignupParams) error {
	email := NormalizeEmail(p.Email)
	if err := ValidateEmail(email); err != nil {
		return err
	}
	if err := ValidatePassword(p.Password); err != nil {
		return err
	}

	// Hashing happens before the address is looked up, so the expensive part
	// of the request runs identically on both paths. Doing it only for new
	// addresses would make an existing one answer measurably faster and give
	// away exactly what the identical responses are hiding.
	hash, err := s.hasher.Hash(p.Password)
	if err != nil {
		return err
	}

	displayName := p.DisplayName
	if displayName == "" {
		displayName = defaultDisplayName(email)
	}

	user, accountID, err := s.repo.CreateUserWithAccount(ctx, CreateUserParams{
		Email:             email,
		PasswordHash:      hash,
		DisplayName:       displayName,
		PasswordChangedAt: s.clock.Now(),
	})
	if err != nil {
		if errors.Is(err, ErrEmailTaken) {
			// Deliberately indistinguishable from success. Logged so an
			// operator can still see the attempt.
			s.log.Info("signup for an address that already exists")
			return nil
		}
		return err
	}

	s.log.Info("account created",
		slog.String("user_id", string(user.ID)),
		slog.String("account_id", string(accountID)),
	)

	// Consent is recorded against the versions that were on screen. Storing
	// the version rather than a boolean is what makes it possible to answer,
	// later, which text this person actually agreed to.
	for _, doc := range []string{DocumentTermsOfService, DocumentPrivacyPolicy} {
		version := s.consentVersions[doc]
		if version == "" {
			continue
		}
		if err := s.repo.RecordConsent(ctx, user.ID, doc, version, p.IP); err != nil {
			// A missing consent row is a compliance gap, not a reason to fail
			// a sign-up that already committed.
			s.log.Error("recording consent failed",
				slog.String("document", doc),
				slog.String("error", err.Error()))
		}
	}

	s.audit(ctx, AuthEvent{
		UserID: user.ID, Email: email, Type: EventSignup,
		IP: p.IP, UserAgent: p.UserAgent,
	})

	if err := s.sendVerificationEmail(ctx, user.ID, email); err != nil {
		// The account exists and the person can request a new link, so this is
		// worth an alert rather than an error that suggests nothing happened.
		s.log.Error("sending verification email failed",
			slog.String("error", err.Error()))
	}

	return nil
}

// sendVerificationEmail mints a verification token and delivers it.
//
// Only the digest is stored. The token itself exists in the message and
// nowhere else, so a disclosure of the token table cannot be turned into a
// set of usable links.
func (s *Service) sendVerificationEmail(ctx context.Context, userID id.UserID, email string) error {
	token, err := crypto.NewToken()
	if err != nil {
		return err
	}

	expiresAt := s.clock.Now().Add(s.emailTokenLifetime)
	if err := s.repo.CreateToken(ctx, userID, PurposeEmailVerify, token.Digest(), expiresAt); err != nil {
		return err
	}

	return s.mailer.SendEmailVerification(ctx, email, token.Plaintext())
}

// VerifyEmail confirms an address from an emailed link.
//
// The token is claimed and validated in one statement, so a link forwarded to
// a second reader cannot be used twice.
func (s *Service) VerifyEmail(ctx context.Context, plaintext, ip, userAgent string) error {
	if plaintext == "" {
		return ErrTokenInvalid
	}

	now := s.clock.Now()

	userID, err := s.repo.ConsumeToken(ctx, PurposeEmailVerify, crypto.DigestToken(plaintext), now)
	if err != nil {
		return err
	}

	if err := s.repo.MarkEmailVerified(ctx, userID, now); err != nil {
		return err
	}

	s.audit(ctx, AuthEvent{
		UserID: userID, Type: EventEmailVerified,
		IP: ip, UserAgent: userAgent,
	})

	return nil
}

// audit appends to the authentication trail.
//
// A failure here is logged at error and never returned. The trail is
// security-relevant, so losing an entry deserves attention, but refusing a
// login because the audit write failed would turn a database hiccup into an
// outage, and refusing a *failed* login would be no protection at all.
func (s *Service) audit(ctx context.Context, e AuthEvent) {
	if err := s.repo.RecordAuthEvent(ctx, e); err != nil {
		s.log.Error("recording auth event failed",
			slog.String("type", string(e.Type)),
			slog.String("error", err.Error()))
	}
}
