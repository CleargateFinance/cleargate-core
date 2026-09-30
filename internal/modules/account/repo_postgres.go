package account

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
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
			INSERT INTO app_user (principal_id, email, password_hash, password_changed_at)
			VALUES ($1, $2, $3, $4)
			RETURNING id, password_changed_at, status, created_at`,
			principalID, p.Email, p.PasswordHash, p.PasswordChangedAt,
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

	u, err := r.scanUser(r.db.QueryRow(ctx, q, email))
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrCredentialsInvalid
	}
	if err != nil {
		return User{}, fmt.Errorf("account: load user: %w", err)
	}
	return u, nil
}

// scanUser reads one app_user row.
//
// It passes pgx.ErrNoRows through untouched, because callers disagree about
// what an absent user means: on the login path it must be indistinguishable
// from a wrong password, while a session pointing at one is simply an invalid
// session.
func (r *postgresRepo) scanUser(row pgx.Row) (User, error) {
	var (
		u         User
		userID    string
		principal string
	)

	if err := row.Scan(
		&userID, &principal, &u.Email, &u.EmailVerifiedAt, &u.PasswordHash,
		&u.PasswordChangedAt, &u.Status, &u.CreatedAt,
	); err != nil {
		return User{}, err
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

// CreateMFAFactor stores a new, unconfirmed factor.
//
// Any earlier enrolment of the same type that was never confirmed is removed
// first, in the same transaction. Abandoning an enrolment halfway is ordinary —
// a QR code scanned on a laptop the person has since closed — and leaving the
// old rows behind would make "the pending factor" ambiguous.
//
// A confirmed factor is left untouched, and the partial unique index on
// (user_id, type) refuses a second one, so this cannot quietly replace a
// working second factor with one an attacker controls.
func (r *postgresRepo) CreateMFAFactor(
	ctx context.Context,
	userID id.UserID,
	factorType MFAFactorType,
	secretEnc []byte,
) (MFAFactor, error) {
	factor := MFAFactor{UserID: userID, Type: factorType, SecretEnc: secretEnc}

	err := r.db.UoW.Do(ctx, func(ctx context.Context) error {
		if _, err := r.db.Exec(ctx, `
			DELETE FROM user_mfa_factor
			WHERE user_id = $1 AND type = $2
			  AND confirmed_at IS NULL AND revoked_at IS NULL`,
			string(userID), string(factorType),
		); err != nil {
			return fmt.Errorf("account: clear pending mfa factor: %w", err)
		}

		var factorID string
		if err := r.db.QueryRow(ctx, `
			INSERT INTO user_mfa_factor (user_id, type, secret_enc)
			VALUES ($1, $2, $3)
			RETURNING id, created_at`,
			string(userID), string(factorType), secretEnc,
		).Scan(&factorID, &factor.CreatedAt); err != nil {
			if isUniqueViolation(err) {
				return ErrMFAAlreadyEnrolled
			}
			return fmt.Errorf("account: create mfa factor: %w", err)
		}

		factor.ID = factorID
		return nil
	})
	if err != nil {
		return MFAFactor{}, err
	}

	return factor, nil
}

// MFAFactor returns the live factor of a type, confirmed or not.
//
// Ordering puts a confirmed factor first, so a pending enrolment started
// alongside an existing one can never shadow the factor that actually works.
func (r *postgresRepo) MFAFactor(
	ctx context.Context,
	userID id.UserID,
	factorType MFAFactorType,
) (MFAFactor, error) {
	const q = `
		SELECT id, user_id, type, secret_enc, last_used_step, confirmed_at,
		       created_at, revoked_at
		FROM user_mfa_factor
		WHERE user_id = $1 AND type = $2 AND revoked_at IS NULL
		ORDER BY confirmed_at DESC NULLS LAST, created_at DESC
		LIMIT 1`

	var (
		factor   MFAFactor
		factorID string
		ownerID  string
		kind     string
	)

	err := r.db.QueryRow(ctx, q, string(userID), string(factorType)).Scan(
		&factorID, &ownerID, &kind, &factor.SecretEnc, &factor.LastUsedStep,
		&factor.ConfirmedAt, &factor.CreatedAt, &factor.RevokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return MFAFactor{}, ErrMFANotEnrolled
	}
	if err != nil {
		return MFAFactor{}, fmt.Errorf("account: load mfa factor: %w", err)
	}

	factor.ID = factorID
	factor.UserID = id.UserID(ownerID)
	factor.Type = MFAFactorType(kind)
	return factor, nil
}

// ConfirmMFAFactor completes enrolment and installs the recovery codes.
//
// Both happen in one transaction because neither is any use alone: codes
// without a confirmed factor protect nothing, and a confirmed factor without
// codes turns a lost phone into a lost account.
//
// Any codes from a previous enrolment are discarded. Leaving them usable would
// mean a list printed against an old factor still opens the account after the
// factor it belonged to is gone.
func (r *postgresRepo) ConfirmMFAFactor(
	ctx context.Context,
	factorID string,
	userID id.UserID,
	at time.Time,
	codeHashes []string,
) error {
	return r.db.UoW.Do(ctx, func(ctx context.Context) error {
		tag, err := r.db.Exec(ctx, `
			UPDATE user_mfa_factor
			SET confirmed_at = $2
			WHERE id = $1 AND confirmed_at IS NULL AND revoked_at IS NULL`,
			factorID, at)
		if err != nil {
			if isUniqueViolation(err) {
				// The partial unique index refused a second confirmed factor of
				// this type, which means one was confirmed by a concurrent
				// request between this caller's check and this write.
				return ErrMFAAlreadyEnrolled
			}
			return fmt.Errorf("account: confirm mfa factor: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrMFAEnrolmentNotStarted
		}

		if _, err := r.db.Exec(ctx,
			`DELETE FROM user_recovery_code WHERE user_id = $1`, string(userID),
		); err != nil {
			return fmt.Errorf("account: clear recovery codes: %w", err)
		}

		// One statement rather than ten round trips, which matters because this
		// runs inside a transaction holding locks.
		if _, err := r.db.Exec(ctx, `
			INSERT INTO user_recovery_code (user_id, code_hash)
			SELECT $1, hash FROM unnest($2::text[]) AS hash`,
			string(userID), codeHashes,
		); err != nil {
			return fmt.Errorf("account: create recovery codes: %w", err)
		}

		return nil
	})
}

// ClaimTOTPStep records a counter step and reports whether this caller claimed
// it.
//
// The condition is what makes a code single-use. A code is valid for its whole
// window, so somebody who observes one can present it again until the window
// closes. Refusing any step at or below the highest already accepted turns that
// window into a single opportunity.
//
// Checking and writing in one statement matters as much as the condition: two
// requests carrying the same captured code would otherwise both read the old
// step, both decide the code was fresh, and both be let through.
func (r *postgresRepo) ClaimTOTPStep(ctx context.Context, factorID string, step int64) (bool, error) {
	const q = `
		UPDATE user_mfa_factor
		SET last_used_step = $2
		WHERE id = $1
		  AND revoked_at IS NULL
		  AND (last_used_step IS NULL OR last_used_step < $2)`

	tag, err := r.db.Exec(ctx, q, factorID, step)
	if err != nil {
		return false, fmt.Errorf("account: claim totp step: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// HasConfirmedMFA reports whether a user holds any usable second factor.
func (r *postgresRepo) HasConfirmedMFA(ctx context.Context, userID id.UserID) (bool, error) {
	const q = `
		SELECT EXISTS (
			SELECT 1 FROM user_mfa_factor
			WHERE user_id = $1 AND confirmed_at IS NOT NULL AND revoked_at IS NULL
		)`

	var enrolled bool
	if err := r.db.QueryRow(ctx, q, string(userID)).Scan(&enrolled); err != nil {
		return false, fmt.Errorf("account: check mfa enrolment: %w", err)
	}
	return enrolled, nil
}

// UnusedRecoveryCodes returns the hashes still available to a user.
//
// Only hashes come back. Verification is a linear scan over these, which is
// deliberate: the codes are argon2id-hashed, so there is no digest to index by,
// and the alternative — a cheap lookup hash — would put a fast-to-brute-force
// value in the same table as the slow one.
func (r *postgresRepo) UnusedRecoveryCodes(ctx context.Context, userID id.UserID) ([]RecoveryCode, error) {
	const q = `
		SELECT id, code_hash
		FROM user_recovery_code
		WHERE user_id = $1 AND used_at IS NULL
		ORDER BY created_at`

	rows, err := r.db.Query(ctx, q, string(userID))
	if err != nil {
		return nil, fmt.Errorf("account: load recovery codes: %w", err)
	}
	defer rows.Close()

	var codes []RecoveryCode
	for rows.Next() {
		var code RecoveryCode
		if err := rows.Scan(&code.ID, &code.Hash); err != nil {
			return nil, fmt.Errorf("account: scan recovery code: %w", err)
		}
		codes = append(codes, code)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account: load recovery codes: %w", err)
	}

	return codes, nil
}

// ConsumeRecoveryCode claims one code and reports whether this caller got it.
//
// Conditional on the code still being unused, so two requests carrying the same
// code cannot both be let through.
func (r *postgresRepo) ConsumeRecoveryCode(ctx context.Context, codeID string, at time.Time) (bool, error) {
	const q = `
		UPDATE user_recovery_code
		SET used_at = $2
		WHERE id = $1 AND used_at IS NULL`

	tag, err := r.db.Exec(ctx, q, codeID, at)
	if err != nil {
		return false, fmt.Errorf("account: consume recovery code: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkSessionMFASatisfied stamps the moment a second factor was presented,
// which is what the step-up window is measured from.
func (r *postgresRepo) MarkSessionMFASatisfied(ctx context.Context, digest []byte, at time.Time) error {
	const q = `
		UPDATE user_session
		SET mfa_satisfied_at = $2
		WHERE token_hash = $1 AND revoked_at IS NULL`

	if _, err := r.db.Exec(ctx, q, digest, at); err != nil {
		return fmt.Errorf("account: mark session mfa satisfied: %w", err)
	}
	return nil
}

// MembershipRole returns what a user may do inside one account.
//
// A user who is not a member at all gets ErrNotPermitted rather than a
// not-found, because the two are the same answer to the only question a caller
// is asking, and distinguishing them would let somebody probe which accounts a
// colleague belongs to.
func (r *postgresRepo) MembershipRole(ctx context.Context, accountID id.AccountID, userID id.UserID) (PermissionRole, error) {
	const q = `SELECT role FROM account_membership WHERE account_id = $1 AND user_id = $2`

	var role string
	err := r.db.QueryRow(ctx, q, string(accountID), string(userID)).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotPermitted
	}
	if err != nil {
		return "", fmt.Errorf("account: load membership role: %w", err)
	}
	return PermissionRole(role), nil
}

// AccountHasGuardian reports whether a guardian is already appointed.
func (r *postgresRepo) AccountHasGuardian(ctx context.Context, accountID id.AccountID) (bool, error) {
	const q = `
		SELECT EXISTS (
			SELECT 1 FROM account_membership
			WHERE account_id = $1 AND role = 'guardian'
		)`

	var exists bool
	if err := r.db.QueryRow(ctx, q, string(accountID)).Scan(&exists); err != nil {
		return false, fmt.Errorf("account: check guardian: %w", err)
	}
	return exists, nil
}

// IsMember reports whether an address already belongs to the account.
func (r *postgresRepo) IsMember(ctx context.Context, accountID id.AccountID, email string) (bool, error) {
	const q = `
		SELECT EXISTS (
			SELECT 1 FROM account_membership m
			JOIN app_user u ON u.id = m.user_id
			WHERE m.account_id = $1 AND u.email = $2
		)`

	var exists bool
	if err := r.db.QueryRow(ctx, q, string(accountID), email).Scan(&exists); err != nil {
		return false, fmt.Errorf("account: check membership: %w", err)
	}
	return exists, nil
}

// UserByID loads a login by its identifier.
func (r *postgresRepo) UserByID(ctx context.Context, userID id.UserID) (User, error) {
	const q = `
		SELECT id, principal_id, email, email_verified_at, password_hash,
		       password_changed_at, status, created_at
		FROM app_user
		WHERE id = $1`

	u, err := r.scanUser(r.db.QueryRow(ctx, q, string(userID)))
	if errors.Is(err, pgx.ErrNoRows) {
		// A session naming a user who is not there is not a user problem, it is
		// a session that cannot authorise anything.
		return User{}, ErrSessionInvalid
	}
	if err != nil {
		return User{}, fmt.Errorf("account: load user: %w", err)
	}
	return u, nil
}

// CreateInvitation stores an offer of membership and the digest of its token.
func (r *postgresRepo) CreateInvitation(ctx context.Context, inv Invitation, digest []byte) (Invitation, error) {
	const q = `
		WITH created AS (
			INSERT INTO account_invitation
				(account_id, email, role, invited_by, token_hash, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id, account_id, created_at
		)
		SELECT created.id, created.created_at, a.display_name
		FROM created JOIN account a ON a.id = created.account_id`

	err := r.db.QueryRow(ctx, q,
		string(inv.AccountID), inv.Email, string(inv.Role),
		string(inv.InvitedBy), digest, inv.ExpiresAt,
	).Scan(&inv.ID, &inv.CreatedAt, &inv.AccountName)
	if err != nil {
		if isUniqueViolation(err) {
			// The partial unique index refused a second outstanding invitation
			// for this address. Resending is revoke-then-invite, so that the
			// first link stops working rather than both staying live.
			return Invitation{}, ErrAlreadyInvited
		}
		return Invitation{}, fmt.Errorf("account: create invitation: %w", err)
	}

	return inv, nil
}

// InvitationByDigest reads an invitation without claiming it.
func (r *postgresRepo) InvitationByDigest(ctx context.Context, digest []byte) (Invitation, error) {
	const q = `
		SELECT i.id, i.account_id, i.email, i.role, i.invited_by, a.display_name,
		       i.expires_at, i.accepted_at, i.revoked_at, i.created_at
		FROM account_invitation i
		JOIN account a ON a.id = i.account_id
		WHERE i.token_hash = $1
		  AND i.accepted_at IS NULL
		  AND i.revoked_at IS NULL
		  AND i.expires_at > now()`

	return r.scanInvitation(r.db.QueryRow(ctx, q, digest))
}

// AcceptInvitationAsMember claims an invitation and grants the membership.
func (r *postgresRepo) AcceptInvitationAsMember(
	ctx context.Context,
	digest []byte,
	userID id.UserID,
	at time.Time,
) (Invitation, error) {
	var invitation Invitation

	err := r.db.UoW.Do(ctx, func(ctx context.Context) error {
		claimed, err := r.claimInvitation(ctx, digest, at)
		if err != nil {
			return err
		}

		if err := r.insertMembershipFor(ctx, claimed, userID); err != nil {
			return err
		}

		invitation = claimed
		return nil
	})
	if err != nil {
		return Invitation{}, err
	}

	return invitation, nil
}

// AcceptInvitationAsNewUser claims an invitation, creates the invitee's login,
// and grants the membership, all together.
func (r *postgresRepo) AcceptInvitationAsNewUser(
	ctx context.Context,
	digest []byte,
	p CreateUserParams,
	at time.Time,
) (User, Invitation, error) {
	var (
		user       User
		invitation Invitation
	)

	err := r.db.UoW.Do(ctx, func(ctx context.Context) error {
		claimed, err := r.claimInvitation(ctx, digest, at)
		if err != nil {
			return err
		}

		var principalID string
		if err := r.db.QueryRow(ctx,
			`INSERT INTO principal (kind) VALUES ('person') RETURNING id`,
		).Scan(&principalID); err != nil {
			return fmt.Errorf("account: create principal: %w", err)
		}

		if _, err := r.db.Exec(ctx,
			`INSERT INTO principal_identity (principal_id) VALUES ($1)`, principalID,
		); err != nil {
			return fmt.Errorf("account: create principal identity: %w", err)
		}

		// email_verified_at is stamped here rather than left for a separate
		// message. Opening a link sent to this address is the proof that
		// verification asks for, and asking again would be asking twice.
		var userID string
		err = r.db.QueryRow(ctx, `
			INSERT INTO app_user (principal_id, email, password_hash,
			                      password_changed_at, email_verified_at)
			VALUES ($1, $2, $3, $4, $4)
			RETURNING id, email_verified_at, password_changed_at, status, created_at`,
			principalID, p.Email, p.PasswordHash, p.PasswordChangedAt,
		).Scan(&userID, &user.EmailVerifiedAt, &user.PasswordChangedAt,
			&user.Status, &user.CreatedAt)
		if err != nil {
			if isUniqueViolation(err) {
				// Somebody registered this address between the invitation being
				// read and this transaction. They should accept as themselves.
				return ErrEmailTaken
			}
			return fmt.Errorf("account: create user: %w", err)
		}

		user.ID = id.UserID(userID)
		user.PrincipalID = id.PrincipalID(principalID)
		user.Email = p.Email
		user.PasswordHash = p.PasswordHash

		claimed.Email = p.Email
		if err := r.insertMembershipFor(ctx, claimed, user.ID); err != nil {
			return err
		}

		invitation = claimed
		return nil
	})
	if err != nil {
		return User{}, Invitation{}, err
	}

	return user, invitation, nil
}

// claimInvitation marks an invitation accepted, in one statement.
//
// Every condition is in the UPDATE rather than in a preceding SELECT. Reading
// first and writing second would let two people opening the same forwarded link
// both pass the check before either wrote, and both end up inside the account.
func (r *postgresRepo) claimInvitation(ctx context.Context, digest []byte, at time.Time) (Invitation, error) {
	const q = `
		UPDATE account_invitation
		SET accepted_at = $2
		WHERE token_hash = $1
		  AND accepted_at IS NULL
		  AND revoked_at IS NULL
		  AND expires_at > $2
		RETURNING id, account_id, email, role, invited_by, expires_at,
		          accepted_at, revoked_at, created_at`

	var (
		inv       Invitation
		accountID string
		invitedBy string
		role      string
	)

	err := r.db.QueryRow(ctx, q, digest, at).Scan(
		&inv.ID, &accountID, &inv.Email, &role, &invitedBy, &inv.ExpiresAt,
		&inv.AcceptedAt, &inv.RevokedAt, &inv.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		// Unknown, expired, revoked and already accepted are indistinguishable
		// here, which is what stops a link from being probed.
		return Invitation{}, ErrInvitationInvalid
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("account: accept invitation: %w", err)
	}

	inv.AccountID = id.AccountID(accountID)
	inv.InvitedBy = id.UserID(invitedBy)
	inv.Role = PermissionRole(role)
	return inv, nil
}

// insertMembershipFor grants an invitation's role to a specific user.
func (r *postgresRepo) insertMembershipFor(ctx context.Context, inv Invitation, userID id.UserID) error {
	// ON CONFLICT DO NOTHING covers somebody who was already a member by
	// another route. The invitation is still spent, which is correct: it was
	// used, and leaving it live would let it be used again.
	const q = `
		INSERT INTO account_membership (account_id, user_id, role, invited_by)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (account_id, user_id) DO NOTHING`

	if _, err := r.db.Exec(ctx, q,
		string(inv.AccountID), string(userID), string(inv.Role), string(inv.InvitedBy),
	); err != nil {
		if isUniqueViolation(err) {
			// The partial unique index refused a second guardian, which means
			// one was appointed between the check and this write.
			return ErrGuardianRequiresControlChange
		}
		return fmt.Errorf("account: create membership: %w", err)
	}
	return nil
}

// RevokeInvitation withdraws an outstanding offer.
//
// Scoped to the account as well as the identifier, so an identifier guessed or
// leaked from one account cannot be used to cancel another's invitations.
func (r *postgresRepo) RevokeInvitation(
	ctx context.Context,
	invitationID string,
	accountID id.AccountID,
	at time.Time,
) (bool, error) {
	const q = `
		UPDATE account_invitation
		SET revoked_at = $3
		WHERE id = $1 AND account_id = $2
		  AND accepted_at IS NULL AND revoked_at IS NULL`

	tag, err := r.db.Exec(ctx, q, invitationID, string(accountID), at)
	if err != nil {
		return false, fmt.Errorf("account: revoke invitation: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RemoveMembership revokes a role.
func (r *postgresRepo) RemoveMembership(ctx context.Context, accountID id.AccountID, userID id.UserID) (bool, error) {
	const q = `DELETE FROM account_membership WHERE account_id = $1 AND user_id = $2`

	tag, err := r.db.Exec(ctx, q, string(accountID), string(userID))
	if err != nil {
		return false, fmt.Errorf("account: remove membership: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// UpdateMembershipRole changes an existing member's role.
func (r *postgresRepo) UpdateMembershipRole(
	ctx context.Context,
	accountID id.AccountID,
	userID id.UserID,
	role PermissionRole,
) (bool, error) {
	const q = `
		UPDATE account_membership SET role = $3
		WHERE account_id = $1 AND user_id = $2`

	tag, err := r.db.Exec(ctx, q, string(accountID), string(userID), string(role))
	if err != nil {
		if isUniqueViolation(err) {
			return false, ErrGuardianRequiresControlChange
		}
		return false, fmt.Errorf("account: update membership role: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// CreateAgent registers an agent against an account.
func (r *postgresRepo) CreateAgent(
	ctx context.Context,
	accountID id.AccountID,
	name string,
	createdBy id.UserID,
) (Agent, error) {
	const q = `
		INSERT INTO agent (account_id, name, created_by)
		VALUES ($1, $2, $3)
		RETURNING id, status, created_at`

	agent := Agent{AccountID: accountID, Name: name, CreatedBy: createdBy}

	var agentID string
	var status string
	if err := r.db.QueryRow(ctx, q, string(accountID), name, string(createdBy)).
		Scan(&agentID, &status, &agent.CreatedAt); err != nil {
		return Agent{}, fmt.Errorf("account: create agent: %w", err)
	}

	agent.ID = id.AgentID(agentID)
	agent.Status = AgentStatus(status)
	return agent, nil
}

// Agent loads one registration.
func (r *postgresRepo) Agent(ctx context.Context, agentID id.AgentID) (Agent, error) {
	const q = `
		SELECT id, account_id, name, status, created_by, created_at, revoked_at
		FROM agent
		WHERE id = $1`

	var (
		agent     Agent
		rawID     string
		accountID string
		status    string
		createdBy *string
	)

	err := r.db.QueryRow(ctx, q, string(agentID)).Scan(
		&rawID, &accountID, &agent.Name, &status, &createdBy,
		&agent.CreatedAt, &agent.RevokedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Agent{}, ErrAgentNotFound
	}
	if err != nil {
		return Agent{}, fmt.Errorf("account: load agent: %w", err)
	}

	agent.ID = id.AgentID(rawID)
	agent.AccountID = id.AccountID(accountID)
	agent.Status = AgentStatus(status)
	if createdBy != nil {
		agent.CreatedBy = id.UserID(*createdBy)
	}
	return agent, nil
}

// RevokeAgent retires an agent and every credential it holds.
//
// Both in one transaction. An agent marked revoked whose keys stayed live is
// precisely the state somebody containing an incident believes they have just
// eliminated, and the gap would be invisible until one of those keys was used.
func (r *postgresRepo) RevokeAgent(ctx context.Context, agentID id.AgentID, at time.Time) error {
	return r.db.UoW.Do(ctx, func(ctx context.Context) error {
		if _, err := r.db.Exec(ctx, `
			UPDATE agent SET status = 'revoked', revoked_at = $2
			WHERE id = $1 AND revoked_at IS NULL`,
			string(agentID), at,
		); err != nil {
			return fmt.Errorf("account: revoke agent: %w", err)
		}

		if _, err := r.db.Exec(ctx, `
			UPDATE agent_credential SET revoked_at = $2
			WHERE agent_id = $1 AND revoked_at IS NULL`,
			string(agentID), at,
		); err != nil {
			return fmt.Errorf("account: revoke agent credentials: %w", err)
		}

		return nil
	})
}

// CreateAgentCredential stores a key's handle and the digest of its secret.
func (r *postgresRepo) CreateAgentCredential(
	ctx context.Context,
	agentID id.AgentID,
	name, prefix string,
	digest []byte,
	createdBy id.UserID,
) (AgentCredential, error) {
	const q = `
		INSERT INTO agent_credential (agent_id, name, prefix, secret_hash, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at`

	credential := AgentCredential{
		AgentID: agentID, Name: name, Prefix: prefix, CreatedBy: createdBy,
	}

	if err := r.db.QueryRow(ctx, q,
		string(agentID), name, prefix, digest, string(createdBy),
	).Scan(&credential.ID, &credential.CreatedAt); err != nil {
		return AgentCredential{}, fmt.Errorf("account: create agent credential: %w", err)
	}

	return credential, nil
}

// ActiveCredentialByPrefix resolves a handle to the identity behind it.
//
// The join is what makes revoking an agent take effect on every key it holds
// without a sweep, and every validity condition sits in the query rather than
// in Go, so no caller can hold an identity that looks usable but is not.
func (r *postgresRepo) ActiveCredentialByPrefix(ctx context.Context, prefix string) (AgentIdentity, []byte, error) {
	const q = `
		SELECT c.id, c.secret_hash, a.id, a.account_id
		FROM agent_credential c
		JOIN agent a ON a.id = c.agent_id
		WHERE c.prefix = $1
		  AND c.revoked_at IS NULL
		  AND a.status = 'active'`

	var (
		identity  AgentIdentity
		digest    []byte
		agentID   string
		accountID string
	)

	err := r.db.QueryRow(ctx, q, prefix).Scan(
		&identity.CredentialID, &digest, &agentID, &accountID)
	if errors.Is(err, pgx.ErrNoRows) {
		return AgentIdentity{}, nil, ErrCredentialInvalid
	}
	if err != nil {
		return AgentIdentity{}, nil, fmt.Errorf("account: load agent credential: %w", err)
	}

	identity.AgentID = id.AgentID(agentID)
	identity.AccountID = id.AccountID(accountID)
	identity.Prefix = prefix
	return identity, digest, nil
}

// AgentCredentials lists an agent's keys, secrets excluded.
func (r *postgresRepo) AgentCredentials(ctx context.Context, agentID id.AgentID) ([]AgentCredential, error) {
	const q = `
		SELECT id, name, prefix, created_by, created_at, revoked_at, last_used_at
		FROM agent_credential
		WHERE agent_id = $1
		ORDER BY created_at`

	rows, err := r.db.Query(ctx, q, string(agentID))
	if err != nil {
		return nil, fmt.Errorf("account: list agent credentials: %w", err)
	}
	defer rows.Close()

	var credentials []AgentCredential
	for rows.Next() {
		credential := AgentCredential{AgentID: agentID}
		var createdBy *string

		if err := rows.Scan(&credential.ID, &credential.Name, &credential.Prefix,
			&createdBy, &credential.CreatedAt, &credential.RevokedAt,
			&credential.LastUsedAt); err != nil {
			return nil, fmt.Errorf("account: scan agent credential: %w", err)
		}
		if createdBy != nil {
			credential.CreatedBy = id.UserID(*createdBy)
		}

		credentials = append(credentials, credential)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account: list agent credentials: %w", err)
	}

	return credentials, nil
}

// RevokeAgentCredential retires one key.
func (r *postgresRepo) RevokeAgentCredential(
	ctx context.Context,
	agentID id.AgentID,
	credentialID string,
	at time.Time,
) (bool, error) {
	const q = `
		UPDATE agent_credential SET revoked_at = $3
		WHERE id = $1 AND agent_id = $2 AND revoked_at IS NULL`

	tag, err := r.db.Exec(ctx, q, credentialID, string(agentID), at)
	if err != nil {
		return false, fmt.Errorf("account: revoke agent credential: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// TouchAgentCredential records that a key was used.
func (r *postgresRepo) TouchAgentCredential(ctx context.Context, credentialID string, at time.Time) error {
	const q = `UPDATE agent_credential SET last_used_at = $2 WHERE id = $1`

	if _, err := r.db.Exec(ctx, q, credentialID, at); err != nil {
		return fmt.Errorf("account: touch agent credential: %w", err)
	}
	return nil
}

// CreateAgentSession opens a run.
func (r *postgresRepo) CreateAgentSession(
	ctx context.Context,
	agentID id.AgentID,
	taskRef, taskText string,
) (AgentSession, error) {
	const q = `
		INSERT INTO agent_session (agent_id, task_ref, task_text)
		VALUES ($1, $2, $3)
		RETURNING id, started_at`

	session := AgentSession{AgentID: agentID, TaskRef: taskRef, TaskText: taskText}

	// Empty strings go in as NULL, because "the caller did not say" is a real
	// state and storing it as an empty string makes it indistinguishable from
	// somebody who said nothing on purpose.
	var ref, text any
	if taskRef != "" {
		ref = taskRef
	}
	if taskText != "" {
		text = taskText
	}

	var sessionID string
	if err := r.db.QueryRow(ctx, q, string(agentID), ref, text).
		Scan(&sessionID, &session.StartedAt); err != nil {
		return AgentSession{}, fmt.Errorf("account: create agent session: %w", err)
	}

	session.ID = id.AgentSessionID(sessionID)
	return session, nil
}

// scanInvitation reads one invitation row.
func (r *postgresRepo) scanInvitation(row pgx.Row) (Invitation, error) {
	var (
		inv       Invitation
		accountID string
		invitedBy string
		role      string
	)

	err := row.Scan(&inv.ID, &accountID, &inv.Email, &role, &invitedBy,
		&inv.AccountName, &inv.ExpiresAt, &inv.AcceptedAt, &inv.RevokedAt, &inv.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Invitation{}, ErrInvitationInvalid
	}
	if err != nil {
		return Invitation{}, fmt.Errorf("account: load invitation: %w", err)
	}

	inv.AccountID = id.AccountID(accountID)
	inv.InvitedBy = id.UserID(invitedBy)
	inv.Role = PermissionRole(role)
	return inv, nil
}

// VerificationTier returns what a principal is currently entitled to do.
func (r *postgresRepo) VerificationTier(ctx context.Context, principalID id.PrincipalID) (VerificationTier, error) {
	const q = `SELECT verification_tier FROM principal_identity WHERE principal_id = $1`

	var tier string
	err := r.db.QueryRow(ctx, q, string(principalID)).Scan(&tier)
	if errors.Is(err, pgx.ErrNoRows) {
		// No compliance record means nothing has been established, which is
		// exactly what the basic tier means. Treating it as an error would make
		// every caller handle a case that is simply the starting state.
		return TierBasic, nil
	}
	if err != nil {
		return "", fmt.Errorf("account: load verification tier: %w", err)
	}
	return VerificationTier(tier), nil
}

// SetVerificationTier records a new entitlement.
func (r *postgresRepo) SetVerificationTier(ctx context.Context, principalID id.PrincipalID, tier VerificationTier) error {
	const q = `UPDATE principal_identity SET verification_tier = $2 WHERE principal_id = $1`

	tag, err := r.db.Exec(ctx, q, string(principalID), string(tier))
	if err != nil {
		// The database refuses a tier above basic without a verified kyc_status,
		// so this is a caller claiming entitlement it has not established.
		return fmt.Errorf("account: set verification tier: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return apperr.NotFound("account: no compliance record for this principal")
	}
	return nil
}

// ScreenedAddress returns the verdict on one address for one principal.
func (r *postgresRepo) ScreenedAddress(
	ctx context.Context,
	principalID id.PrincipalID,
	chain, address string,
) (ScreenedAddress, error) {
	const q = `
		SELECT id, principal_id, chain, address, status, provider, reference,
		       screened_at, first_funded_at, created_at
		FROM screened_address
		WHERE principal_id = $1 AND chain = $2 AND address = $3`

	screened, err := r.scanScreenedAddress(r.db.QueryRow(ctx, q, string(principalID), chain, address))
	if errors.Is(err, pgx.ErrNoRows) {
		return ScreenedAddress{}, ErrAddressNotScreened
	}
	if err != nil {
		return ScreenedAddress{}, fmt.Errorf("account: load screened address: %w", err)
	}
	return screened, nil
}

// RecordScreening stores a screening verdict.
//
// Upserted on (principal, chain, address) because screening is periodic: an
// address clear last month can be flagged today, and the current verdict is
// what governs. first_funded_at is preserved across a re-screen, since whether
// an address funded this principal is a historical fact that a later verdict
// does not change.
func (r *postgresRepo) RecordScreening(ctx context.Context, a ScreenedAddress) (ScreenedAddress, error) {
	const q = `
		INSERT INTO screened_address
			(principal_id, chain, address, status, provider, reference, screened_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (principal_id, chain, address) DO UPDATE
		SET status      = EXCLUDED.status,
		    provider    = EXCLUDED.provider,
		    reference   = EXCLUDED.reference,
		    screened_at = EXCLUDED.screened_at
		RETURNING id, principal_id, chain, address, status, provider, reference,
		          screened_at, first_funded_at, created_at`

	var provider, reference any
	if a.Provider != "" {
		provider = a.Provider
	}
	if a.Reference != "" {
		reference = a.Reference
	}

	screened, err := r.scanScreenedAddress(r.db.QueryRow(ctx, q,
		string(a.PrincipalID), a.Chain, a.Address, string(a.Status),
		provider, reference, a.ScreenedAt))
	if err != nil {
		return ScreenedAddress{}, fmt.Errorf("account: record screening: %w", err)
	}
	return screened, nil
}

// MarkAddressFunded stamps the first deposit from an address.
//
// Only the first: the column answers "has this address ever funded us", and
// moving it on every later deposit would lose the answer to when the
// relationship began without gaining anything.
func (r *postgresRepo) MarkAddressFunded(
	ctx context.Context,
	principalID id.PrincipalID,
	chain, address string,
	at time.Time,
) error {
	const q = `
		UPDATE screened_address SET first_funded_at = $4
		WHERE principal_id = $1 AND chain = $2 AND address = $3
		  AND first_funded_at IS NULL`

	if _, err := r.db.Exec(ctx, q, string(principalID), chain, address, at); err != nil {
		return fmt.Errorf("account: mark address funded: %w", err)
	}
	return nil
}

// FundingAddresses lists the addresses that have actually funded a principal.
func (r *postgresRepo) FundingAddresses(ctx context.Context, principalID id.PrincipalID) ([]ScreenedAddress, error) {
	const q = `
		SELECT id, principal_id, chain, address, status, provider, reference,
		       screened_at, first_funded_at, created_at
		FROM screened_address
		WHERE principal_id = $1 AND first_funded_at IS NOT NULL
		ORDER BY first_funded_at`

	rows, err := r.db.Query(ctx, q, string(principalID))
	if err != nil {
		return nil, fmt.Errorf("account: list funding addresses: %w", err)
	}
	defer rows.Close()

	var addresses []ScreenedAddress
	for rows.Next() {
		screened, err := r.scanScreenedAddress(rows)
		if err != nil {
			return nil, fmt.Errorf("account: scan funding address: %w", err)
		}
		addresses = append(addresses, screened)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("account: list funding addresses: %w", err)
	}
	return addresses, nil
}

// scanScreenedAddress reads one screening row.
func (r *postgresRepo) scanScreenedAddress(row pgx.Row) (ScreenedAddress, error) {
	var (
		a           ScreenedAddress
		principalID string
		status      string
		provider    *string
		reference   *string
	)

	if err := row.Scan(&a.ID, &principalID, &a.Chain, &a.Address, &status,
		&provider, &reference, &a.ScreenedAt, &a.FirstFundedAt, &a.CreatedAt); err != nil {
		return ScreenedAddress{}, err
	}

	a.PrincipalID = id.PrincipalID(principalID)
	a.Status = ScreeningStatus(status)
	if provider != nil {
		a.Provider = *provider
	}
	if reference != nil {
		a.Reference = *reference
	}
	return a, nil
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
