package account

import (
	"context"
	"time"
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

// Clock supplies the current time. Injecting it keeps session expiry and the
// step-up window testable without waiting for real time to pass.
type Clock interface {
	Now() time.Time
}
