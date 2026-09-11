package account

import (
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// MinPasswordLength is the shortest password accepted.
//
// Length is the only rule. Composition requirements — a digit, a symbol, mixed
// case — reliably push people toward predictable shapes like Password1! that
// are weaker than a longer passphrase, and they are the reason password rules
// are hated. Twelve characters with no other constraint is both stronger in
// practice and easier to comply with.
const MinPasswordLength = 12

// MaxEmailLength bounds the address a caller may submit, matching the practical
// limit on a deliverable address. It exists so an unauthenticated endpoint
// cannot be handed a megabyte to normalise and index.
const MaxEmailLength = 254

// UserStatus is whether a login may be used.
type UserStatus string

const (
	// UserActive can sign in.
	UserActive UserStatus = "active"
	// UserSuspended is blocked, reversibly, usually by an operator.
	UserSuspended UserStatus = "suspended"
	// UserDeactivated has been closed by the person themselves.
	UserDeactivated UserStatus = "deactivated"
)

// User is one human login.
type User struct {
	ID          id.UserID
	PrincipalID id.PrincipalID

	Email string
	// EmailVerifiedAt is nil until the address has been confirmed. Sign-in
	// requires it, so a typo'd or borrowed address cannot become a live login.
	EmailVerifiedAt *time.Time

	PasswordHash string
	// PasswordChangedAt is what invalidates sessions on a password change:
	// anything issued before this instant stops validating.
	PasswordChangedAt time.Time

	Status    UserStatus
	CreatedAt time.Time
}

// EmailVerified reports whether the address has been confirmed.
func (u User) EmailVerified() bool { return u.EmailVerifiedAt != nil }

// TokenPurpose is what a single-use emailed token authorises.
type TokenPurpose string

const (
	// PurposeEmailVerify confirms an address at sign-up.
	PurposeEmailVerify TokenPurpose = "email_verify"
	// PurposePasswordReset authorises setting a new password.
	PurposePasswordReset TokenPurpose = "password_reset"
)

// AuthEventType names something worth recording about an authentication
// attempt. The set matches what the database will accept.
type AuthEventType string

// An authentication recorded outcome.
const (
	EventSignup                 AuthEventType = "signup"
	EventEmailVerified          AuthEventType = "email_verified"
	EventLoginSucceeded         AuthEventType = "login_succeeded"
	EventLoginFailed            AuthEventType = "login_failed"
	EventPasswordResetRequested AuthEventType = "password_reset_requested"
	EventPasswordChanged        AuthEventType = "password_changed"
	EventSessionRevoked         AuthEventType = "session_revoked"
	EventMFAEnrolled            AuthEventType = "mfa_enrolled"
	EventMFASucceeded           AuthEventType = "mfa_succeeded"
	EventMFAFailed              AuthEventType = "mfa_failed"
	EventRecoveryCodeUsed       AuthEventType = "recovery_code_used"
	EventInviteAccepted         AuthEventType = "invite_accepted"
)

// AuthEvent is one recorded authentication outcome.
//
// UserID is empty when the attempt named an address that does not exist, which
// is why the column is nullable: those attempts are the most interesting ones
// in a credential-stuffing run and must still be recorded.
type AuthEvent struct {
	UserID    id.UserID
	Email     string
	Type      AuthEventType
	IP        string
	UserAgent string
}

// Errors returned by the authentication flows.
//
// ErrCredentialsInvalid covers both "no such address" and "wrong password" on
// purpose. Anything that distinguishes them turns the login endpoint into a
// tool for checking which addresses on a leaked list belong to people holding
// balances here.
//
// The two that follow it are safe to be specific about, because reaching them
// already required the correct password. Telling someone their address is
// unverified reveals nothing they have not already proven they know.
var (
	ErrCredentialsInvalid = apperr.Unauthorized("email or password is incorrect")
	ErrEmailNotVerified   = apperr.Forbidden("email address has not been verified")
	ErrUserSuspended      = apperr.Forbidden("this login is suspended")

	// ErrTokenInvalid covers unknown, expired and already-used tokens
	// identically, for the same reason ErrCredentialsInvalid is undivided.
	ErrTokenInvalid = apperr.Unauthorized("this link is invalid or has expired")

	// ErrEmailTaken is internal to this package. It travels from the store up
	// to the sign-up flow, which swallows it and answers exactly as it would
	// for a fresh address. It must never reach a caller.
	ErrEmailTaken = apperr.Conflict("email is already registered")
)

// ThrottledError reports that the caller must wait before trying again.
//
// It carries the delay so a transport can put it in a Retry-After header, and
// wraps a rate-limited application error so the usual error classification
// still finds the right kind.
type ThrottledError struct {
	RetryAfter time.Duration
	cause      error
}

func newThrottled(retryAfter time.Duration) *ThrottledError {
	return &ThrottledError{
		RetryAfter: retryAfter,
		cause:      apperr.RateLimited("too many attempts, please wait before trying again"),
	}
}

func (e *ThrottledError) Error() string { return e.cause.Error() }

// Unwrap exposes the wrapped application error to errors.Is and errors.As, and
// through them to the kind-to-status mapping.
func (e *ThrottledError) Unwrap() error { return e.cause }

// NormalizeEmail puts an address into the one form that will be stored.
//
// Case folding happens here and is also enforced by a database constraint. The
// application does it so the common path is friendly; the database does it so
// a forgotten call site cannot register both Anna@example.com and
// anna@example.com and leave one of them unable to sign in.
func NormalizeEmail(address string) string {
	return strings.ToLower(strings.TrimSpace(address))
}

// ValidateEmail reports whether an address is one that could be delivered to.
//
// The parse is deliberately the standard library's, not a regular expression.
// Address syntax is far more permissive than the patterns people write for it,
// and a homegrown expression's failure mode is rejecting somebody's real
// address with no recourse.
func ValidateEmail(address string) error {
	if address == "" {
		return errInvalid("email is required")
	}
	if len(address) > MaxEmailLength {
		return errInvalid("email is too long")
	}

	parsed, err := mail.ParseAddress(address)
	if err != nil || parsed.Address != address {
		// A mismatch means the input carried a display name or surrounding
		// whitespace. Only the bare address is acceptable.
		return errInvalid("email is not a valid address")
	}
	if strings.Count(address, "@") != 1 {
		return errInvalid("email is not a valid address")
	}

	return nil
}

// ValidatePassword reports whether a password may be used.
//
// Length is counted in runes rather than bytes, so a passphrase in a non-Latin
// script is not penalised for encoding to more bytes per character.
func ValidatePassword(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return errInvalid(fmt.Sprintf("password must be at least %d characters", MinPasswordLength))
	}
	if len(password) > crypto.MaxPasswordLength {
		return errInvalid("password is too long")
	}
	return nil
}

// defaultDisplayName derives an account name from an address.
//
// It is a starting value the owner can change, not an identifier, so the local
// part is good enough and avoids asking one more question during sign-up.
func defaultDisplayName(email string) string {
	local, _, found := strings.Cut(email, "@")
	if !found || local == "" {
		return email
	}
	return local
}
