package account

import (
	"context"
	"errors"
	"log/slog"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// RequestPasswordReset emails a reset link, if the address belongs to anyone.
//
// Like sign-up, it answers the same way whether or not the address exists, for
// the same reason: this endpoint would otherwise confirm which addresses on a
// leaked list are customers here. A caller cannot tell from the result whether
// a message was sent.
func (s *Service) RequestPasswordReset(ctx context.Context, address, ip, userAgent string) error {
	email := NormalizeEmail(address)
	key := ThrottleKey("reset", ip, email)

	if wait, err := s.throttle.RetryAfter(ctx, key); err == nil && wait > 0 {
		return newThrottled(wait)
	}

	// Every request counts against the throttle, successful or not. Only
	// counting known addresses would make the throttle itself an oracle: an
	// attacker could tell a real address from a fake one by which of them
	// eventually started being rate-limited.
	if _, err := s.throttle.Fail(ctx, key); err != nil {
		s.log.Warn("recording reset request in the throttle failed")
	}

	user, err := s.repo.UserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, ErrCredentialsInvalid) {
			s.log.Info("password reset requested for an unknown address")
			return nil
		}
		return err
	}

	token, err := crypto.NewToken()
	if err != nil {
		return err
	}

	expiresAt := s.clock.Now().Add(s.resetTokenLifetime)
	if err := s.repo.CreateToken(ctx, user.ID, PurposePasswordReset, token.Digest(), expiresAt); err != nil {
		return err
	}

	s.audit(ctx, AuthEvent{
		UserID: user.ID, Email: email, Type: EventPasswordResetRequested,
		IP: ip, UserAgent: userAgent,
	})

	if err := s.mailer.SendPasswordReset(ctx, email, token.Plaintext()); err != nil {
		s.log.Error("sending password reset email failed",
			slog.String("error", err.Error()))
	}

	return nil
}

// ResetPassword sets a new password from an emailed link.
//
// Claiming the token is a single statement, so two requests carrying the same
// link cannot both succeed — which matters here more than anywhere else,
// because a replayable reset is a reset an attacker can race the owner for.
func (s *Service) ResetPassword(ctx context.Context, plaintext, newPassword, ip, userAgent string) error {
	if plaintext == "" {
		return ErrTokenInvalid
	}
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}

	hash, err := s.hasher.Hash(newPassword)
	if err != nil {
		return err
	}

	now := s.clock.Now()

	userID, err := s.repo.ConsumeToken(ctx, PurposePasswordReset, crypto.DigestToken(plaintext), now)
	if err != nil {
		return err
	}

	// Moving password_changed_at is what actually ends every existing session:
	// validation refuses anything issued before it. That happens atomically,
	// covering even a session created by a race during this very request,
	// which a delete-the-rows approach could miss.
	if err := s.repo.UpdatePassword(ctx, userID, hash, now); err != nil {
		return err
	}

	// The timestamp above is authoritative and enough on its own. This second
	// step exists only for immediacy: without it, a session already warm in
	// the cache keeps working until its entry expires. Every failure here is
	// therefore survivable, because correctness never depended on it.
	s.clearUserSessions(ctx, userID)

	s.audit(ctx, AuthEvent{
		UserID: userID, Type: EventPasswordChanged,
		IP: ip, UserAgent: userAgent,
	})

	return nil
}

// clearUserSessions revokes every live session for a user and drops the cached
// entries that would otherwise keep them working until they expired.
//
// Purely an immediacy measure. password_changed_at already invalidates those
// sessions at the database, so every failure below is logged and swallowed.
func (s *Service) clearUserSessions(ctx context.Context, userID id.UserID) {
	digests, err := s.repo.RevokeUserSessions(ctx, userID)
	if err != nil {
		s.log.Warn("revoking sessions after a password change failed",
			slog.String("error", err.Error()))
		return
	}

	for _, digest := range digests {
		if err := s.cache.Delete(ctx, sessionCacheKey(digest)); err != nil {
			s.log.Warn("clearing a cached session after a password change failed",
				slog.String("error", err.Error()))
		}
	}
}
