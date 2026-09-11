package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

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

// RevokeUserSessions ends every live session for one user and returns their
// digests so the caller can clear the matching cache entries.
//
// The digests come back from the same statement that revokes them, rather than
// from a separate select, so a session created between the two cannot be
// missed.
func (r *postgresRepo) RevokeUserSessions(ctx context.Context, userID id.UserID) ([][]byte, error) {
	const q = `
		UPDATE user_session
		SET revoked_at = now()
		WHERE user_id = $1 AND revoked_at IS NULL
		RETURNING token_hash`

	rows, err := r.db.Query(ctx, q, string(userID))
	if err != nil {
		return nil, fmt.Errorf("account: revoke user sessions: %w", err)
	}
	defer rows.Close()

	var digests [][]byte
	for rows.Next() {
		var digest []byte
		if err := rows.Scan(&digest); err != nil {
			return nil, fmt.Errorf("account: scan revoked session: %w", err)
		}
		digests = append(digests, digest)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account: iterate revoked sessions: %w", err)
	}

	return digests, nil
}

// CreateUserWithAccount writes the whole of sign-up in one transaction.
//
// Five rows have to appear together or not at all. A principal with no login,
// a login with no account, or an account with no owner are all states that
// nothing downstream knows how to repair, and each would need a person to
// unpick by hand.
func (r *postgresRepo) CreateUserWithAccount(ctx context.Context, p CreateUserParams) (User, id.AccountID, error) {
	var (
		user      User
		accountID id.AccountID
	)

	err := r.db.UoW.Do(ctx, func(ctx context.Context) error {
		var principalID string
		if err := r.db.QueryRow(ctx,
			`INSERT INTO principal (kind) VALUES ('person') RETURNING id`,
		).Scan(&principalID); err != nil {
			return fmt.Errorf("account: create principal: %w", err)
		}

		// The compliance record starts empty. It gains a legal name and a
		// verification status only if this principal ever funds an account.
		if _, err := r.db.Exec(ctx,
			`INSERT INTO principal_identity (principal_id) VALUES ($1)`, principalID,
		); err != nil {
			return fmt.Errorf("account: create principal identity: %w", err)
		}

		var userID string
		err := r.db.QueryRow(ctx, `
			INSERT INTO app_user (principal_id, email, password_hash)
			VALUES ($1, $2, $3)
			RETURNING id, password_changed_at, status, created_at`,
			principalID, p.Email, p.PasswordHash,
		).Scan(&userID, &user.PasswordChangedAt, &user.Status, &user.CreatedAt)
		if err != nil {
			if isUniqueViolation(err) {
				// The address is taken. The caller turns this into the same
				// answer an unused address receives.
				return ErrEmailTaken
			}
			return fmt.Errorf("account: create user: %w", err)
		}

		var accID string
		if err := r.db.QueryRow(ctx, `
			INSERT INTO account (principal_id, display_name)
			VALUES ($1, $2)
			RETURNING id`,
			principalID, p.DisplayName,
		).Scan(&accID); err != nil {
			return fmt.Errorf("account: create account: %w", err)
		}

		// The person who signs up owns what they created. A partial unique
		// index keeps it that way: there is exactly one owner per account.
		if _, err := r.db.Exec(ctx, `
			INSERT INTO account_membership (account_id, user_id, role)
			VALUES ($1, $2, 'owner')`,
			accID, userID,
		); err != nil {
			return fmt.Errorf("account: create owner membership: %w", err)
		}

		user.ID = id.UserID(userID)
		user.PrincipalID = id.PrincipalID(principalID)
		user.Email = p.Email
		user.PasswordHash = p.PasswordHash
		accountID = id.AccountID(accID)
		return nil
	})
	if err != nil {
		return User{}, "", err
	}

	return user, accountID, nil
}

// UserByEmail loads a login by its normalised address.
func (r *postgresRepo) UserByEmail(ctx context.Context, email string) (User, error) {
	const q = `
		SELECT id, principal_id, email, email_verified_at, password_hash,
		       password_changed_at, status, created_at
		FROM app_user
		WHERE email = $1`

	var (
		u         User
		userID    string
		principal string
	)

	err := r.db.QueryRow(ctx, q, email).Scan(
		&userID, &principal, &u.Email, &u.EmailVerifiedAt, &u.PasswordHash,
		&u.PasswordChangedAt, &u.Status, &u.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrCredentialsInvalid
	}
	if err != nil {
		return User{}, fmt.Errorf("account: load user: %w", err)
	}

	u.ID = id.UserID(userID)
	u.PrincipalID = id.PrincipalID(principal)
	return u, nil
}

// MarkEmailVerified records that an address has been confirmed.
func (r *postgresRepo) MarkEmailVerified(ctx context.Context, userID id.UserID, at time.Time) error {
	// Only the first confirmation counts. Leaving the original timestamp in
	// place keeps it meaningful as "when this address was proven".
	const q = `
		UPDATE app_user
		SET email_verified_at = $2
		WHERE id = $1 AND email_verified_at IS NULL`

	if _, err := r.db.Exec(ctx, q, string(userID), at); err != nil {
		return fmt.Errorf("account: mark email verified: %w", err)
	}
	return nil
}

// UpdatePassword replaces the hash and moves password_changed_at forward.
//
// That timestamp is what ends every existing session: session validation
// refuses anything issued before it, atomically and without a delete that
// could miss a row created by a race.
func (r *postgresRepo) UpdatePassword(ctx context.Context, userID id.UserID, hash string, at time.Time) error {
	const q = `
		UPDATE app_user
		SET password_hash = $2, password_changed_at = $3
		WHERE id = $1`

	if _, err := r.db.Exec(ctx, q, string(userID), hash, at); err != nil {
		return fmt.Errorf("account: update password: %w", err)
	}
	return nil
}

// RecordConsent stores acceptance of one version of one document.
//
// Re-accepting a version already on file is ignored rather than duplicated:
// what matters for a later question is that this person agreed to this text,
// not how many times they clicked.
func (r *postgresRepo) RecordConsent(ctx context.Context, userID id.UserID, document, version, ip string) error {
	const q = `
		INSERT INTO user_consent (user_id, document, version, ip)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, document, version) DO NOTHING`

	var clientIP any
	if ip != "" {
		clientIP = ip
	}

	if _, err := r.db.Exec(ctx, q, string(userID), document, version, clientIP); err != nil {
		return fmt.Errorf("account: record consent: %w", err)
	}
	return nil
}

// CreateToken stores a single-use emailed token.
func (r *postgresRepo) CreateToken(
	ctx context.Context,
	userID id.UserID,
	purpose TokenPurpose,
	digest []byte,
	expiresAt time.Time,
) error {
	const q = `
		INSERT INTO user_token (user_id, purpose, token_hash, expires_at)
		VALUES ($1, $2, $3, $4)`

	if _, err := r.db.Exec(ctx, q, string(userID), string(purpose), digest, expiresAt); err != nil {
		return fmt.Errorf("account: create token: %w", err)
	}
	return nil
}

// ConsumeToken claims a token and reports whose it was.
//
// Claiming and validating happen in the same statement on purpose. Checking
// first and updating second would let two requests carrying the same link both
// pass the check before either wrote, and a password reset that can be
// replayed is a password reset an attacker can race the owner for.
func (r *postgresRepo) ConsumeToken(
	ctx context.Context,
	purpose TokenPurpose,
	digest []byte,
	at time.Time,
) (id.UserID, error) {
	const q = `
		UPDATE user_token
		SET consumed_at = $3
		WHERE token_hash = $1
		  AND purpose = $2
		  AND consumed_at IS NULL
		  AND expires_at > $3
		RETURNING user_id`

	var userID string
	err := r.db.QueryRow(ctx, q, digest, string(purpose), at).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Unknown, expired and already used are indistinguishable here, which
		// is what stops a link from being probed.
		return "", ErrTokenInvalid
	}
	if err != nil {
		return "", fmt.Errorf("account: consume token: %w", err)
	}

	return id.UserID(userID), nil
}

// RecordAuthEvent appends to the authentication audit trail.
func (r *postgresRepo) RecordAuthEvent(ctx context.Context, e AuthEvent) error {
	const q = `
		INSERT INTO auth_event (user_id, email, type, ip, user_agent)
		VALUES ($1, $2, $3, $4, $5)`

	// Every optional column is a real absence rather than an empty string: an
	// attempt against an address nobody owns has no user, and a client may
	// present neither an address we can parse nor an agent string.
	var userID any
	if e.UserID != "" {
		userID = string(e.UserID)
	}
	var email any
	if e.Email != "" {
		email = e.Email
	}
	var ip any
	if e.IP != "" {
		ip = e.IP
	}
	var userAgent any
	if e.UserAgent != "" {
		userAgent = e.UserAgent
	}

	if _, err := r.db.Exec(ctx, q, userID, email, string(e.Type), ip, userAgent); err != nil {
		return fmt.Errorf("account: record auth event: %w", err)
	}
	return nil
}

// isUniqueViolation reports whether err is Postgres rejecting a duplicate.
//
// The code is checked rather than the message, because messages are localised
// and change between releases.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// UpdatePasswordHashOnly replaces the stored hash without touching
// password_changed_at.
//
// Used when a hash is upgraded to stronger parameters. The password is the
// same one the person already knows, so their other devices have no reason to
// be signed out.
func (r *postgresRepo) UpdatePasswordHashOnly(ctx context.Context, userID id.UserID, hash string) error {
	const q = `UPDATE app_user SET password_hash = $2 WHERE id = $1`

	if _, err := r.db.Exec(ctx, q, string(userID), hash); err != nil {
		return fmt.Errorf("account: update password hash: %w", err)
	}
	return nil
}
