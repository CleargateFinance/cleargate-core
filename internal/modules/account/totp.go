package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// sealContext binds a sealed secret to the factor it belongs to.
//
// It is supplied again when opening, and a mismatch fails authentication. That
// is what stops a ciphertext lifted from one row and written into another from
// decrypting: an attacker with write access to the factor table cannot install
// a secret they already know against somebody else's user.
func sealContext(userID id.UserID, factorType MFAFactorType) string {
	return fmt.Sprintf("mfa:%s:%s", factorType, userID)
}

// BeginTOTPEnrolment generates a shared secret and stores it, unconfirmed.
//
// Nothing is protected yet at this point, deliberately. The factor becomes
// usable only once ConfirmTOTPEnrolment proves the person's app is producing
// codes that match, which is what stops a mistyped secret, a wrong timezone or
// an abandoned enrolment from leaving somebody locked out of their own account
// by a factor they cannot satisfy.
//
// Starting again simply replaces any pending secret. Re-enrolling over a
// *confirmed* factor is refused: that is a credential replacement, and it runs
// through removal, which is itself step-up gated.
func (s *Service) BeginTOTPEnrolment(ctx context.Context, userID id.UserID, accountName string) (crypto.TOTPEnrolment, error) {
	if userID == "" {
		return crypto.TOTPEnrolment{}, errInvalid("enrolment needs a user")
	}

	existing, err := s.repo.MFAFactor(ctx, userID, FactorTOTP)
	switch {
	case err == nil && existing.Confirmed():
		return crypto.TOTPEnrolment{}, ErrMFAAlreadyEnrolled
	case err != nil && !errors.Is(err, ErrMFANotEnrolled):
		return crypto.TOTPEnrolment{}, err
	}

	generated, err := crypto.NewTOTPEnrolment(s.mfaIssuer, accountName)
	if err != nil {
		return crypto.TOTPEnrolment{}, err
	}

	sealed, err := s.sealer.Seal([]byte(generated.Secret), sealContext(userID, FactorTOTP))
	if err != nil {
		return crypto.TOTPEnrolment{}, err
	}

	if _, err := s.repo.CreateMFAFactor(ctx, userID, FactorTOTP, sealed); err != nil {
		return crypto.TOTPEnrolment{}, err
	}

	return generated, nil
}

// ConfirmTOTPEnrolment completes enrolment and returns the recovery codes.
//
// The codes are generated here and returned exactly once. Only their hashes are
// stored, so this return value is the only copy that will ever exist outside
// whatever the person writes them down in, and there is no path by which the
// server can show them again.
func (s *Service) ConfirmTOTPEnrolment(ctx context.Context, userID id.UserID, code, ip, userAgent string) ([]string, error) {
	factor, err := s.repo.MFAFactor(ctx, userID, FactorTOTP)
	if err != nil {
		if errors.Is(err, ErrMFANotEnrolled) {
			return nil, ErrMFAEnrolmentNotStarted
		}
		return nil, err
	}
	if factor.Confirmed() {
		return nil, ErrMFAAlreadyEnrolled
	}

	claimed, err := s.verifyTOTPCode(ctx, factor, code)
	if err != nil || !claimed {
		s.audit(ctx, AuthEvent{
			UserID: userID, Type: EventMFAFailed, IP: ip, UserAgent: userAgent,
		})
		if err != nil {
			return nil, err
		}
		return nil, ErrMFACodeInvalid
	}

	codes, err := NewRecoveryCodes()
	if err != nil {
		return nil, err
	}

	hashes := make([]string, 0, len(codes))
	for _, c := range codes {
		hash, err := s.hasher.Hash(c)
		if err != nil {
			return nil, err
		}
		hashes = append(hashes, hash)
	}

	if err := s.repo.ConfirmMFAFactor(ctx, factor.ID, userID, s.clock.Now(), hashes); err != nil {
		return nil, err
	}

	s.audit(ctx, AuthEvent{
		UserID: userID, Type: EventMFAEnrolled, IP: ip, UserAgent: userAgent,
	})

	return codes, nil
}

// SatisfyTOTP verifies a code against a session and starts the step-up window.
//
// The session is what carries the result. A verified code stamps
// mfa_satisfied_at, and every gated action then asks how long ago that was
// rather than prompting again for each one.
func (s *Service) SatisfyTOTP(ctx context.Context, plaintext, code, ip, userAgent string) (Session, error) {
	session, err := s.ValidateSession(ctx, plaintext)
	if err != nil {
		return Session{}, err
	}

	throttleKey := ThrottleKey("mfa", ip, string(session.UserID))
	if wait, err := s.throttle.RetryAfter(ctx, throttleKey); err == nil && wait > 0 {
		return Session{}, newThrottled(wait)
	}

	factor, err := s.repo.MFAFactor(ctx, session.UserID, FactorTOTP)
	if err != nil {
		return Session{}, err
	}
	if !factor.Confirmed() {
		// A pending enrolment is not a factor. Answering ErrMFANotEnrolled keeps
		// this path indistinguishable from having no factor at all.
		return Session{}, ErrMFANotEnrolled
	}

	claimed, err := s.verifyTOTPCode(ctx, factor, code)
	if err != nil || !claimed {
		s.failMFA(ctx, throttleKey, AuthEvent{
			UserID: session.UserID, Type: EventMFAFailed, IP: ip, UserAgent: userAgent,
		})
		if err != nil {
			return Session{}, err
		}
		return Session{}, ErrMFACodeInvalid
	}

	return s.satisfySession(ctx, session, throttleKey, AuthEvent{
		UserID: session.UserID, Type: EventMFASucceeded, IP: ip, UserAgent: userAgent,
	})
}

// SatisfyRecoveryCode spends one recovery code in place of a TOTP code.
//
// This is the path somebody takes when their phone is gone, so it must work
// without any device they previously held. Each code is single-use, which is
// what keeps a list found on paper from being worth more than one login.
func (s *Service) SatisfyRecoveryCode(ctx context.Context, plaintext, code, ip, userAgent string) (Session, error) {
	session, err := s.ValidateSession(ctx, plaintext)
	if err != nil {
		return Session{}, err
	}

	throttleKey := ThrottleKey("mfa", ip, string(session.UserID))
	if wait, err := s.throttle.RetryAfter(ctx, throttleKey); err == nil && wait > 0 {
		return Session{}, newThrottled(wait)
	}

	stored, err := s.repo.UnusedRecoveryCodes(ctx, session.UserID)
	if err != nil {
		return Session{}, err
	}

	normalized := NormalizeRecoveryCode(code)

	for _, candidate := range stored {
		ok, err := s.hasher.Verify(candidate.Hash, normalized)
		if err != nil || !ok {
			continue
		}

		// Claiming is conditional on the code still being unused, so two
		// requests carrying the same code cannot both succeed.
		claimed, err := s.repo.ConsumeRecoveryCode(ctx, candidate.ID, s.clock.Now())
		if err != nil {
			return Session{}, err
		}
		if !claimed {
			break
		}

		return s.satisfySession(ctx, session, throttleKey, AuthEvent{
			UserID: session.UserID, Type: EventRecoveryCodeUsed, IP: ip, UserAgent: userAgent,
		})
	}

	s.failMFA(ctx, throttleKey, AuthEvent{
		UserID: session.UserID, Type: EventMFAFailed, IP: ip, UserAgent: userAgent,
	})
	return Session{}, ErrMFACodeInvalid
}

// verifyTOTPCode opens the sealed secret, checks the code, and claims its step.
//
// It reports whether the step was claimed by this call. A correct code whose
// step was already spent returns false, not an error, so the caller answers
// exactly as it would for a wrong code: a replay must be indistinguishable from
// a guess, or the response tells an attacker that the code they captured was
// genuine.
func (s *Service) verifyTOTPCode(ctx context.Context, factor MFAFactor, code string) (bool, error) {
	secret, err := s.sealer.Open(factor.SecretEnc, sealContext(factor.UserID, factor.Type))
	if err != nil {
		// The row cannot be decrypted, which means a retired master key or a
		// tampered value, not a wrong code. It needs an operator, so it is
		// logged loudly and reported as a failure rather than a bad code.
		s.log.Error("opening mfa secret failed",
			slog.String("user_id", string(factor.UserID)),
			slog.String("error", err.Error()))
		return false, err
	}

	step, ok, err := crypto.VerifyTOTP(string(secret), code, s.clock.Now())
	if err != nil {
		s.log.Error("stored totp secret is unreadable",
			slog.String("user_id", string(factor.UserID)))
		return false, err
	}
	if !ok {
		return false, ErrMFACodeInvalid
	}

	return s.repo.ClaimTOTPStep(ctx, factor.ID, step)
}

// satisfySession stamps the step-up window and clears the throttle.
func (s *Service) satisfySession(ctx context.Context, session Session, throttleKey string, event AuthEvent) (Session, error) {
	now := s.clock.Now()

	if err := s.repo.MarkSessionMFASatisfied(ctx, session.Digest, now); err != nil {
		return Session{}, err
	}

	// The cached copy still says no factor was presented, and a stale entry
	// here would reject the very action the code was entered for. Dropping the
	// key makes the next read come from Postgres.
	if err := s.cache.Delete(ctx, sessionCacheKey(session.Digest)); err != nil {
		s.log.Warn("clearing cached session after mfa failed",
			slog.String("error", err.Error()))
	}

	if err := s.throttle.Succeed(ctx, throttleKey); err != nil {
		s.log.Warn("clearing mfa throttle failed")
	}

	s.audit(ctx, event)

	session.MFASatisfiedAt = &now
	return session, nil
}

// failMFA records an unsuccessful attempt against the throttle and the trail.
//
// Six digits is a small space, and a session cookie alone should not be enough
// to walk it. Backoff is what makes the attempt count that matters unreachable
// long before the code space is.
func (s *Service) failMFA(ctx context.Context, throttleKey string, event AuthEvent) {
	if _, err := s.throttle.Fail(ctx, throttleKey); err != nil {
		s.log.Warn("recording mfa failure in the throttle failed")
	}
	s.audit(ctx, event)
}
