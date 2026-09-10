package account

import (
	"context"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Repository is the storage this module needs.
//
// Declared here, by the consumer, rather than by whatever implements it, so
// Service depends only on this shape and the Postgres implementation can be
// swapped for a fake in tests without service code changing.
type Repository interface {
	// CreateSession stores a new session and returns it as persisted, with the
	// identifier and timestamps the database assigned.
	CreateSession(ctx context.Context, s Session) (Session, error)

	// ActiveSessionByDigest returns the session for this token digest, if one
	// exists and may currently authorise a request.
	//
	// The validity conditions live in the query rather than in Go, so a
	// revoked, expired or suspended session is simply not returned. Callers
	// receive ErrSessionInvalid and cannot forget a check.
	ActiveSessionByDigest(ctx context.Context, digest []byte) (Session, error)

	// RevokeSession marks a session revoked. Revoking an already revoked or
	// unknown session is not an error, so signing out twice is harmless.
	RevokeSession(ctx context.Context, digest []byte) error

	// TouchSession records that a session was used, for the "your active
	// sessions" view. It is advisory: failing to record a visit must never
	// fail the request that was otherwise authorised.
	TouchSession(ctx context.Context, digest []byte, at time.Time) error

	// RevokeUserSessions revokes every live session belonging to one user and
	// returns their digests, so their cached entries can be cleared too.
	RevokeUserSessions(ctx context.Context, userID id.UserID) ([][]byte, error)

	// CreateUserWithAccount performs the whole of sign-up as one unit: the
	// principal, its compliance record, the login, the account, and the
	// owner membership. Either a person gets all of it or none of it, because
	// a login with no account, or an account with no owner, is a state nothing
	// downstream knows how to repair.
	//
	// It reports ErrEmailTaken when the address already exists, which the
	// caller must convert into the same answer an unused address gets.
	CreateUserWithAccount(ctx context.Context, p CreateUserParams) (User, id.AccountID, error)

	// UserByEmail loads a login by its normalised address.
	UserByEmail(ctx context.Context, email string) (User, error)

	// MarkEmailVerified records that an address has been confirmed.
	MarkEmailVerified(ctx context.Context, userID id.UserID, at time.Time) error

	// UpdatePassword replaces a password hash and moves password_changed_at
	// forward, which is what stops sessions issued before it from validating.
	UpdatePassword(ctx context.Context, userID id.UserID, hash string, at time.Time) error

	// UpdatePasswordHashOnly replaces the stored hash and leaves
	// password_changed_at alone.
	//
	// This is for re-hashing under stronger settings, where the password did
	// not actually change. Moving the timestamp would sign the person out of
	// every device as a side effect of an upgrade they never asked for.
	UpdatePasswordHashOnly(ctx context.Context, userID id.UserID, hash string) error

	// RecordConsent stores that a user accepted a version of a document.
	RecordConsent(ctx context.Context, userID id.UserID, document, version, ip string) error

	// CreateToken stores a single-use emailed token.
	CreateToken(ctx context.Context, userID id.UserID, purpose TokenPurpose, digest []byte, expiresAt time.Time) error

	// ConsumeToken claims a token and returns whose it was.
	//
	// Claiming and checking are one statement, not two, so two requests
	// racing on the same link cannot both succeed. It reports
	// ErrTokenInvalid for unknown, expired and already-consumed alike.
	ConsumeToken(ctx context.Context, purpose TokenPurpose, digest []byte, at time.Time) (id.UserID, error)

	// RecordAuthEvent appends to the authentication audit trail.
	RecordAuthEvent(ctx context.Context, e AuthEvent) error
}

// CreateUserParams is everything sign-up needs to write in one transaction.
type CreateUserParams struct {
	Email        string
	PasswordHash string
	DisplayName  string
}

// PasswordHasher derives and checks password hashes.
//
// An interface rather than a direct dependency so tests can run with cheap
// cost parameters, and so raising cost later touches configuration only.
type PasswordHasher interface {
	// Hash derives a new hash, encoded with the parameters used to produce it.
	Hash(password string) (string, error)

	// Verify reports whether password produced encoded, in constant time.
	Verify(encoded, password string) (bool, error)

	// NeedsRehash reports whether encoded was produced with weaker settings
	// than are now current.
	NeedsRehash(encoded string) (bool, error)

	// DummyVerify spends what a real verification spends, and produces
	// nothing. It exists so an unknown address costs what a known one costs.
	DummyVerify()
}

// SessionCache is the read-through cache in front of session lookups.
//
// It is a plain byte store on purpose. This module owns its own key format,
// its own value encoding and its own expiry policy, exactly as it owns its own
// SQL, so the cache implementation stays free of anything domain-specific.
//
// Every method may fail without consequence: the caller treats an error as a
// miss and reads Postgres instead.
type SessionCache interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	Delete(ctx context.Context, key string) error
}

// Mailer delivers the two messages these flows produce.
//
// It takes the raw token rather than a finished link, because where the console
// lives is a deployment concern this module has no business knowing.
type Mailer interface {
	SendEmailVerification(ctx context.Context, to, token string) error
	SendPasswordReset(ctx context.Context, to, token string) error
}

// Throttle rate-limits repeated attempts against one identity.
//
// Every method may fail without consequence. A throttle that failed closed
// would turn a cache outage into a total sign-in outage, which during an
// incident locks out the very people trying to revoke an agent.
type Throttle interface {
	// RetryAfter reports how long the caller must wait, or zero if they may
	// proceed now.
	RetryAfter(ctx context.Context, key string) (time.Duration, error)

	// Fail records an unsuccessful attempt and returns the penalty now in
	// force.
	Fail(ctx context.Context, key string) (time.Duration, error)

	// Succeed clears the record for a key after a successful attempt.
	Succeed(ctx context.Context, key string) error
}

// Clock supplies the current time. Injecting it keeps session expiry and the
// step-up window testable without waiting for real time to pass.
type Clock interface {
	Now() time.Time
}
