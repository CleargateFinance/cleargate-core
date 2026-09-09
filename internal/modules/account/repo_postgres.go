package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// postgresRepo is the only file in this module that knows SQL exists.
//
// Every method runs through database.DB, which joins whatever transaction is
// open on the context, so none of these need to know whether they are part of
// a larger atomic operation.
type postgresRepo struct {
	db *database.DB
}

// NewPostgresRepository builds the Postgres-backed store.
func NewPostgresRepository(db *database.DB) Repository {
	return &postgresRepo{db: db}
}

// CreateSession stores a session and returns it with the values the database
// assigned.
func (r *postgresRepo) CreateSession(ctx context.Context, s Session) (Session, error) {
	const q = `
		INSERT INTO user_session
			(user_id, token_hash, mfa_satisfied_at, issued_at, last_seen_at,
			 expires_at, ip, user_agent)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, issued_at, last_seen_at, expires_at`

	// An empty string is not a valid inet, and an unknown client address is a
	// real state rather than an error, so both optional columns go in as NULL.
	var ip any
	if s.IP != "" {
		ip = s.IP
	}
	var userAgent any
	if s.UserAgent != "" {
		userAgent = s.UserAgent
	}

	var sessionID string
	err := r.db.QueryRow(ctx, q,
		string(s.UserID), s.Digest, s.MFASatisfiedAt,
		s.IssuedAt, s.LastSeenAt, s.ExpiresAt, ip, userAgent,
	).Scan(&sessionID, &s.IssuedAt, &s.LastSeenAt, &s.ExpiresAt)
	if err != nil {
		return Session{}, fmt.Errorf("account: create session: %w", err)
	}

	s.ID = id.UserSessionID(sessionID)
	return s, nil
}

// ActiveSessionByDigest returns a session only if it may currently authorise a
// request.
//
// Every validity condition is in the query rather than in Go. A caller cannot
// forget one, and no code path can hold a Session value that looks usable but
// is not. The conditions are:
//
//   - the session is not revoked, and has not expired;
//   - the user is still active, so suspending an account ends its sessions
//     without a separate sweep;
//   - the session was issued after the password last changed, which is what
//     makes a password change sign every device out atomically, including
//     sessions created by a race during the change itself.
//
// Note that the cached path cannot apply the last two, and accepts staleness
// bounded by the cache window instead.
func (r *postgresRepo) ActiveSessionByDigest(ctx context.Context, digest []byte) (Session, error) {
	const q = `
		SELECT s.id, s.user_id, s.token_hash, s.mfa_satisfied_at,
		       s.issued_at, s.last_seen_at, s.expires_at, s.revoked_at,
		       COALESCE(host(s.ip), ''), COALESCE(s.user_agent, '')
		FROM user_session s
		JOIN app_user u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.revoked_at IS NULL
		  AND s.expires_at > now()
		  AND u.status = 'active'
		  AND s.issued_at >= u.password_changed_at`

	var (
		s         Session
		sessionID string
		userID    string
	)

	err := r.db.QueryRow(ctx, q, digest).Scan(
		&sessionID, &userID, &s.Digest, &s.MFASatisfiedAt,
		&s.IssuedAt, &s.LastSeenAt, &s.ExpiresAt, &s.RevokedAt,
		&s.IP, &s.UserAgent,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// Unknown, revoked, expired and suspended all arrive here, and all
		// return the same error. The caller cannot act on the difference, and
		// reporting it would tell an attacker which tokens once were real.
		return Session{}, ErrSessionInvalid
	}
	if err != nil {
		return Session{}, fmt.Errorf("account: load session: %w", err)
	}

	s.ID = id.UserSessionID(sessionID)
	s.UserID = id.UserID(userID)
	return s, nil
}

// RevokeSession marks a session revoked.
//
// Revoking an unknown or already revoked session is not an error: signing out
// twice, or signing out a session that just expired, is a normal thing for a
// client to do and not a failure worth reporting.
func (r *postgresRepo) RevokeSession(ctx context.Context, digest []byte) error {
	const q = `
		UPDATE user_session
		SET revoked_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL`

	if _, err := r.db.Exec(ctx, q, digest); err != nil {
		return fmt.Errorf("account: revoke session: %w", err)
	}
	return nil
}

// TouchSession records that a session was used.
//
// This feeds the "your active sessions" view, where a stale last-seen value is
// a cosmetic problem. The write is therefore deliberately unconditional and
// cheap, and its failure is not allowed to fail the request.
func (r *postgresRepo) TouchSession(ctx context.Context, digest []byte, at time.Time) error {
	const q = `
		UPDATE user_session
		SET last_seen_at = $2
		WHERE token_hash = $1 AND revoked_at IS NULL`

	if _, err := r.db.Exec(ctx, q, digest, at); err != nil {
		return fmt.Errorf("account: touch session: %w", err)
	}
	return nil
}
