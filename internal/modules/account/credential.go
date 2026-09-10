package account

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// sessionKeyPrefix namespaces session entries in the shared Redis keyspace.
const sessionKeyPrefix = "sess:"

// cachedSession is what a session looks like in Redis.
//
// It carries identity and nothing else. Roles are deliberately absent: cache
// only what deleting the key already invalidates. Session identity is keyed by
// the token, so removing the key covers all of it. A membership role is keyed
// by account and user and changes on its own schedule, so cached here it would
// outlive its own revocation, and demoting an admin would not take effect
// until that person happened to sign out.
type cachedSession struct {
	SessionID      string     `json:"session_id"`
	UserID         string     `json:"user_id"`
	MFASatisfiedAt *time.Time `json:"mfa_satisfied_at,omitempty"`
	IssuedAt       time.Time  `json:"issued_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
}

// sessionCacheKey renders the Redis key for a token digest.
//
// The digest is hex-encoded rather than raw, because a key is a string and raw
// sha256 bytes are not valid UTF-8. Note that what is cached is keyed by the
// digest, never by the token itself, so a dump of Redis is no more useful to
// an attacker than a dump of the sessions table.
func sessionCacheKey(digest []byte) string {
	return sessionKeyPrefix + hex.EncodeToString(digest)
}

// IssueSession mints a bearer token and stores its digest.
//
// The returned token's plaintext is the only copy that will ever exist outside
// the client. It is not written to Postgres, not written to the cache, and not
// logged, so the session table cannot be replayed into live credentials.
func (s *Service) IssueSession(ctx context.Context, p NewSessionParams) (crypto.Token, Session, error) {
	if p.UserID == "" {
		return crypto.Token{}, Session{}, errInvalid("session needs a user")
	}

	token, err := crypto.NewToken()
	if err != nil {
		return crypto.Token{}, Session{}, err
	}

	now := s.clock.Now()

	session := Session{
		UserID:     p.UserID,
		Digest:     token.Digest(),
		IssuedAt:   now,
		LastSeenAt: now,
		ExpiresAt:  now.Add(s.sessionLifetime),
		IP:         p.IP,
		UserAgent:  p.UserAgent,
	}
	if p.MFASatisfied {
		session.MFASatisfiedAt = &now
	}

	stored, err := s.repo.CreateSession(ctx, session)
	if err != nil {
		return crypto.Token{}, Session{}, err
	}

	// The new session is not written to the cache here. It will be populated
	// by the first request that presents it, which keeps one path responsible
	// for filling the cache instead of two that can disagree.
	return token, stored, nil
}

// ValidateSession resolves a presented token to a live session.
//
// Redis is consulted first because this runs on every authenticated request.
// Any cache failure, of any kind, is treated as a miss and falls through to
// Postgres: a cache that failed closed would turn a Redis blip into an
// outage, and a cache treated as authoritative would turn a Redis flush into a
// security incident. Postgres remains the source of truth, so every failure
// mode degrades to the path that was correct before the cache existed.
func (s *Service) ValidateSession(ctx context.Context, plaintext string) (Session, error) {
	if plaintext == "" {
		return Session{}, ErrSessionInvalid
	}

	digest := crypto.DigestToken(plaintext)
	now := s.clock.Now()

	if session, ok := s.sessionFromCache(ctx, digest, now); ok {
		return session, nil
	}

	session, err := s.repo.ActiveSessionByDigest(ctx, digest)
	if err != nil {
		// An unknown token is not cached. The throttle covers hammering, and a
		// negative entry is one more thing that can be wrong.
		return Session{}, err
	}

	s.cacheSession(ctx, session, now)

	// Recording the visit is advisory. A failure here must not fail a request
	// that was otherwise authorised, so the error is logged and dropped.
	if err := s.repo.TouchSession(ctx, digest, now); err != nil {
		s.log.Warn("recording session use failed",
			slog.String("error", err.Error()))
	}

	return session, nil
}

// RevokeSession ends a session immediately.
//
// Postgres is written first and the cache key removed second, and that order
// is what makes revocation instant rather than eventual. Reversing it would
// leave a window where the key is gone, a concurrent request repopulates it
// from a row that is still live, and the session survives its own revocation.
func (s *Service) RevokeSession(ctx context.Context, plaintext string) error {
	if plaintext == "" {
		return ErrSessionInvalid
	}

	digest := crypto.DigestToken(plaintext)

	if err := s.repo.RevokeSession(ctx, digest); err != nil {
		return err
	}

	if err := s.cache.Delete(ctx, sessionCacheKey(digest)); err != nil {
		// The entry now outlives its own revocation, but only until its TTL
		// expires, which is the reason that TTL is short. Worth an alert,
		// not worth failing a sign-out the database already committed.
		s.log.Warn("clearing cached session failed",
			slog.String("error", err.Error()))
	}

	return nil
}

// sessionFromCache attempts to resolve a session without touching Postgres.
//
// A hit still has to be checked against the clock. The entry could have been
// written just before the session expired, and an expiry that the cache would
// otherwise ignore is exactly the kind of staleness this design refuses to
// accept.
func (s *Service) sessionFromCache(ctx context.Context, digest []byte, now time.Time) (Session, bool) {
	raw, found, err := s.cache.Get(ctx, sessionCacheKey(digest))
	if err != nil {
		s.log.Warn("session cache read failed, falling back to the database",
			slog.String("error", err.Error()))
		return Session{}, false
	}
	if !found {
		return Session{}, false
	}

	var entry cachedSession
	if err := json.Unmarshal(raw, &entry); err != nil {
		// A value we cannot parse is a value we cannot trust. Treat it as a
		// miss rather than guessing at it.
		s.log.Warn("session cache entry was unreadable",
			slog.String("error", err.Error()))
		return Session{}, false
	}

	session := Session{
		ID:             id.UserSessionID(entry.SessionID),
		UserID:         id.UserID(entry.UserID),
		Digest:         digest,
		MFASatisfiedAt: entry.MFASatisfiedAt,
		IssuedAt:       entry.IssuedAt,
		ExpiresAt:      entry.ExpiresAt,
	}

	if !session.Active(now) {
		return Session{}, false
	}

	return session, true
}

// cacheSession writes a session to Redis for a bounded time.
//
// The TTL is the shorter of the cache window and the session's own remaining
// life, so an entry can never outlive what it describes. The window is short
// because deleting the key on revocation is what makes revocation immediate,
// and the TTL is the backstop for when that delete does not happen: the call
// errors and something swallows it, the process dies between the commit and
// the delete, or a concurrent request repopulates from a not-yet-committed
// read. With a session-lifetime TTL, a revoked session stays usable for days,
// which is precisely the failure this design exists to avoid.
func (s *Service) cacheSession(ctx context.Context, session Session, now time.Time) {
	ttl := s.cacheWindow
	if remaining := session.ExpiresAt.Sub(now); remaining < ttl {
		ttl = remaining
	}
	if ttl <= 0 {
		return
	}

	value, err := json.Marshal(cachedSession{
		SessionID:      string(session.ID),
		UserID:         string(session.UserID),
		MFASatisfiedAt: session.MFASatisfiedAt,
		IssuedAt:       session.IssuedAt,
		ExpiresAt:      session.ExpiresAt,
	})
	if err != nil {
		s.log.Warn("encoding session for cache failed",
			slog.String("error", err.Error()))
		return
	}

	if err := s.cache.Set(ctx, sessionCacheKey(session.Digest), value, ttl); err != nil {
		// Failing to populate the cache costs a database read next time and
		// nothing else, so it is logged rather than returned.
		s.log.Warn("session cache write failed",
			slog.String("error", err.Error()))
	}
}

// HashPassword derives a storable hash for a password a human chose.
func (s *Service) HashPassword(password string) (string, error) {
	return s.hasher.Hash(password)
}

// VerifyPassword reports whether password matches the stored hash.
//
// Callers must not skip this when the address is unknown. Use
// DummyVerifyPassword in that case, so the two paths cost the same.
func (s *Service) VerifyPassword(encoded, password string) (bool, error) {
	ok, err := s.hasher.Verify(encoded, password)
	if err != nil {
		// A hash we cannot parse means a corrupt or foreign row. It is not a
		// wrong password, and it should not be reported as one.
		if errors.Is(err, crypto.ErrInvalidHash) {
			s.log.Error("stored password hash is unreadable")
		}
		return false, err
	}
	return ok, nil
}

// DummyVerifyPassword spends what a real verification spends and produces
// nothing.
//
// Login, sign-up and password reset all answer identically for known and
// unknown addresses, because otherwise they enumerate which addresses on a
// leaked list belong to people holding balances here. Identical bodies are not
// enough on their own: without this, the unknown-address path returns in
// microseconds while the known one spends tens of milliseconds hashing, and
// the timing says what the body withholds.
func (s *Service) DummyVerifyPassword() {
	s.hasher.DummyVerify()
}

// PasswordNeedsRehash reports whether a stored hash was produced with weaker
// settings than are now current.
//
// Call it directly after a successful verification, which is the one moment
// the plaintext is available to re-derive from. That single opportunistic
// upgrade is the whole migration strategy for raising cost: no batch job, no
// downtime, and no rows left unreadable.
func (s *Service) PasswordNeedsRehash(encoded string) (bool, error) {
	return s.hasher.NeedsRehash(encoded)
}
