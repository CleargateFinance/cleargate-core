package account

import (
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// PermissionRole is what a user may do inside one account.
//
// Roles govern humans only. An agent's authority comes from a mandate instead,
// and the two never mix: a role cannot move money, and a mandate cannot invite
// a colleague.
type PermissionRole string

const (
	// RoleOwner may do anything an admin may, and is additionally the only
	// role that manages admins, appoints or replaces the guardian, and
	// proposes account control changes. Exactly one exists per account,
	// enforced by a partial unique index.
	RoleOwner PermissionRole = "owner"

	// RoleAdmin manages agents and mandates, but not billing, not other
	// admins, and not the guardian. Two admins able to remove each other is a
	// lockout race that an attacker wins by acting first.
	RoleAdmin PermissionRole = "admin"

	// RoleGuardian can cancel and freeze, and can propose nothing. Its power
	// is subtractive on purpose, so a stolen guardian credential is worth
	// nothing on its own.
	RoleGuardian PermissionRole = "guardian"

	// RoleViewer reads the console and reports, and changes nothing.
	RoleViewer PermissionRole = "viewer"
)

// Valid reports whether r is a role the system recognises.
func (r PermissionRole) Valid() bool {
	switch r {
	case RoleOwner, RoleAdmin, RoleGuardian, RoleViewer:
		return true
	default:
		return false
	}
}

// ErrSessionInvalid reports a session token that cannot be used: unknown,
// expired, revoked, or belonging to a user who can no longer sign in.
//
// One error covers every case deliberately. A caller cannot act differently on
// "revoked" than on "never existed", and a specific message on this path tends
// to reach a response body, where it would tell an attacker which tokens once
// were real.
var ErrSessionInvalid = apperr.Unauthorized("session is not valid")

// Session is one human login.
//
// It holds the digest of the bearer token, never the token itself. The
// plaintext exists only in the response that created it and in the client's
// cookie, so a disclosure of this table yields nothing that can be replayed.
type Session struct {
	ID     id.UserSessionID
	UserID id.UserID

	// Digest is sha256 of the presented token, and the lookup key.
	Digest []byte

	// MFASatisfiedAt drives step-up authorization: actions that create
	// authority or move money require this to be recent, not merely present.
	MFASatisfiedAt *time.Time

	IssuedAt   time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	RevokedAt  *time.Time

	// IP and UserAgent are recorded so a person can recognise their own
	// sessions in the console, and spot one that is not theirs.
	IP        string
	UserAgent string
}

// Active reports whether the session may authorise a request at this instant.
//
// The database applies these same conditions inside its lookup. This method
// exists for the cached path, which has no query to apply them in.
func (s Session) Active(now time.Time) bool {
	if s.RevokedAt != nil {
		return false
	}
	return now.Before(s.ExpiresAt)
}

// MFARecent reports whether a second factor was satisfied within window, which
// is what step-up authorization asks before a sensitive action.
func (s Session) MFARecent(now time.Time, window time.Duration) bool {
	if s.MFASatisfiedAt == nil {
		return false
	}
	return s.MFASatisfiedAt.After(now.Add(-window))
}

// NewSessionParams describes a session about to be created.
type NewSessionParams struct {
	UserID id.UserID

	// MFASatisfied records that a second factor was presented during this
	// login, so the step-up window starts now rather than at the next prompt.
	MFASatisfied bool

	IP        string
	UserAgent string
}
