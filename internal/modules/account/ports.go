package account

import (
	"context"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"

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

	// CreateMFAFactor stores a new, unconfirmed factor, replacing any earlier
	// enrolment of the same type that was never completed. Restarting an
	// enrolment is ordinary behaviour, usually because the first QR code was
	// scanned on a device the person no longer has in front of them.
	CreateMFAFactor(ctx context.Context, userID id.UserID, factorType MFAFactorType, secretEnc []byte) (MFAFactor, error)

	// MFAFactor returns the live factor of a type, confirmed or not.
	// It reports ErrMFANotEnrolled when there is none.
	MFAFactor(ctx context.Context, userID id.UserID, factorType MFAFactorType) (MFAFactor, error)

	// ConfirmMFAFactor completes enrolment and replaces the recovery codes in
	// one transaction.
	//
	// The two belong together: codes issued without a confirmed factor protect
	// nothing, and a factor confirmed without codes turns a lost phone into a
	// lost account.
	ConfirmMFAFactor(ctx context.Context, factorID string, userID id.UserID, at time.Time, codeHashes []string) error

	// ClaimTOTPStep records that a counter step has been used, and reports
	// whether this caller was the one that claimed it.
	//
	// Claiming and checking are one statement. Reading the last step and then
	// writing it would let two requests carrying the same captured code both
	// pass the check before either wrote.
	ClaimTOTPStep(ctx context.Context, factorID string, step int64) (bool, error)

	// HasConfirmedMFA reports whether a user holds any usable second factor.
	HasConfirmedMFA(ctx context.Context, userID id.UserID) (bool, error)

	// UnusedRecoveryCodes returns the hashes still available to a user.
	UnusedRecoveryCodes(ctx context.Context, userID id.UserID) ([]RecoveryCode, error)

	// ConsumeRecoveryCode claims one code and reports whether this caller got
	// it, single-statement for the same reason ClaimTOTPStep is.
	ConsumeRecoveryCode(ctx context.Context, codeID string, at time.Time) (bool, error)

	// MarkSessionMFASatisfied stamps a session as having presented a second
	// factor, which is what starts the step-up window.
	MarkSessionMFASatisfied(ctx context.Context, digest []byte, at time.Time) error

	// MembershipRole returns what a user may do inside one account. It reports
	// ErrNotPermitted when they are not a member, so a non-member and a member
	// whose role is too low take the same path.
	MembershipRole(ctx context.Context, accountID id.AccountID, userID id.UserID) (PermissionRole, error)

	// AccountHasGuardian reports whether a guardian is already appointed,
	// which is what separates appointing the first one from replacing it.
	AccountHasGuardian(ctx context.Context, accountID id.AccountID) (bool, error)

	// UserByID loads a login by its identifier.
	UserByID(ctx context.Context, userID id.UserID) (User, error)

	// IsMember reports whether an address already belongs to the account, so
	// an invitation to somebody already present can be refused before a link
	// is sent to them.
	IsMember(ctx context.Context, accountID id.AccountID, email string) (bool, error)

	// CreateInvitation stores an offer of membership and the digest of its
	// token, returning it with the account's name filled in for the message.
	CreateInvitation(ctx context.Context, inv Invitation, digest []byte) (Invitation, error)

	// InvitationByDigest reads an invitation without claiming it, reporting
	// ErrInvitationInvalid unless it may still be accepted.
	//
	// Callers use it to decide what to say before claiming. It is never the
	// authority for whether acceptance may proceed: the claim below is, because
	// only the claim is atomic.
	InvitationByDigest(ctx context.Context, digest []byte) (Invitation, error)

	// AcceptInvitationAsMember claims an invitation and grants the membership,
	// in one transaction.
	//
	// Claiming and validating are a single statement inside it, so two people
	// opening the same forwarded link cannot both be let in, and the membership
	// either accompanies the claim or neither happens.
	AcceptInvitationAsMember(ctx context.Context, digest []byte, userID id.UserID, at time.Time) (Invitation, error)

	// AcceptInvitationAsNewUser claims an invitation, creates the login the
	// invitee does not yet have, and grants the membership, in one transaction.
	//
	// The address is recorded as verified: the person arrived by opening a link
	// sent to it, which is the proof a verification email asks for, so sending
	// one would be asking them to prove the same thing twice.
	//
	// No account of their own is created. They are joining somebody else's
	// account, and an empty unfunded account per invited viewer is noise that
	// reporting and compliance would carry forever.
	AcceptInvitationAsNewUser(ctx context.Context, digest []byte, p CreateUserParams, at time.Time) (User, Invitation, error)

	// RevokeInvitation withdraws an outstanding offer, reporting whether there
	// was one to withdraw.
	RevokeInvitation(ctx context.Context, invitationID string, accountID id.AccountID, at time.Time) (bool, error)

	// RemoveMembership revokes a role, reporting whether there was one.
	RemoveMembership(ctx context.Context, accountID id.AccountID, userID id.UserID) (bool, error)

	// UpdateMembershipRole changes an existing member's role, reporting
	// whether they were a member.
	UpdateMembershipRole(ctx context.Context, accountID id.AccountID, userID id.UserID, role PermissionRole) (bool, error)

	// CreateAgent registers an agent against an account.
	CreateAgent(ctx context.Context, accountID id.AccountID, name string, createdBy id.UserID) (Agent, error)

	// Agent loads one registration, reporting ErrAgentNotFound when there is
	// none.
	Agent(ctx context.Context, agentID id.AgentID) (Agent, error)

	// RevokeAgent retires an agent and every credential it holds, together.
	//
	// One transaction, because an agent marked revoked while its keys stay live
	// is the exact state somebody containing an incident believes they have
	// just eliminated.
	RevokeAgent(ctx context.Context, agentID id.AgentID, at time.Time) error

	// CreateAgentCredential stores a new key's handle and the digest of its
	// secret.
	CreateAgentCredential(ctx context.Context, agentID id.AgentID, name, prefix string, digest []byte, createdBy id.UserID) (AgentCredential, error)

	// ActiveCredentialByPrefix resolves a non-secret handle to the identity
	// behind it, and to the digest the presented secret must match.
	//
	// Every condition that decides whether this credential may authenticate is
	// in the query: the credential is not revoked, and the agent it belongs to
	// is still active. It reports ErrCredentialInvalid when any of them fails,
	// so no caller can forget one.
	ActiveCredentialByPrefix(ctx context.Context, prefix string) (AgentIdentity, []byte, error)

	// AgentCredentials lists an agent's keys, secrets excluded.
	AgentCredentials(ctx context.Context, agentID id.AgentID) ([]AgentCredential, error)

	// RevokeAgentCredential retires one key, reporting whether there was one to
	// retire. Scoped to the agent, so an identifier from elsewhere cannot be
	// used to retire a key that is not this agent's.
	RevokeAgentCredential(ctx context.Context, agentID id.AgentID, credentialID string, at time.Time) (bool, error)

	// TouchAgentCredential records that a key was used. Advisory: failing to
	// record a use must never fail a request that was otherwise authenticated.
	TouchAgentCredential(ctx context.Context, credentialID string, at time.Time) error

	// CreateAgentSession opens a run.
	CreateAgentSession(ctx context.Context, agentID id.AgentID, taskRef, taskText string) (AgentSession, error)

	// VerificationTier returns what a principal is currently entitled to do.
	VerificationTier(ctx context.Context, principalID id.PrincipalID) (VerificationTier, error)

	// SetVerificationTier records a new entitlement.
	//
	// A tier above basic is refused by the database unless kyc_status is
	// verified, so a caller cannot grant entitlement it has not actually
	// established.
	SetVerificationTier(ctx context.Context, principalID id.PrincipalID, tier VerificationTier) error

	// ScreenedAddress returns the verdict on one address for one principal,
	// reporting ErrAddressNotScreened when there is none yet.
	ScreenedAddress(ctx context.Context, principalID id.PrincipalID, chain, address string) (ScreenedAddress, error)

	// RecordScreening stores a screening verdict, creating the record if this
	// address has not been seen for this principal before.
	RecordScreening(ctx context.Context, a ScreenedAddress) (ScreenedAddress, error)

	// MarkAddressFunded stamps the first deposit from an address, which is what
	// return-to-source withdrawal is checked against.
	MarkAddressFunded(ctx context.Context, principalID id.PrincipalID, chain, address string, at time.Time) error

	// FundingAddresses lists the addresses that have actually funded a
	// principal, which is the full set an unverified account may withdraw to.
	FundingAddresses(ctx context.Context, principalID id.PrincipalID) ([]ScreenedAddress, error)
}

// FundingHistory is what this module needs from the ledger to enforce a tier
// cap: how much a principal has funded to date.
//
// A port rather than a query of our own, because the ledger owns that data and
// reading its tables directly from this module's repository would couple two
// modules through a shared database — the exact coupling module boundaries
// exist to prevent. The ledger is also the right authority: it is the system of
// record for money that actually moved, so a cap measured against anything else
// is a cap measured against a number that can disagree with the books.
type FundingHistory interface {
	FundedToDate(ctx context.Context, principalID id.PrincipalID) (currency.Amount, error)
}

// SecretSealer encrypts the few secrets that must be recoverable rather than
// merely comparable.
//
// A port rather than a direct dependency so tests need no real key material,
// and so moving the master key into a managed KMS later is a change of
// implementation rather than a change to any of these flows.
//
// context identifies what the ciphertext belongs to and is re-supplied at Open.
// It is what makes a sealed value non-transferable between rows.
type SecretSealer interface {
	Seal(plaintext []byte, context string) ([]byte, error)
	Open(sealed []byte, context string) ([]byte, error)
}

// CreateUserParams is everything sign-up needs to write in one transaction.
type CreateUserParams struct {
	Email        string
	PasswordHash string
	DisplayName  string

	// PasswordChangedAt is supplied by the caller rather than defaulted by the
	// database, so that it and a session's issued_at come from the same clock.
	// A session is valid only when issued_at >= password_changed_at, and
	// deciding that comparison across two machines' clocks means a new login
	// can be rejected by nothing worse than a few milliseconds of NTP skew.
	PasswordChangedAt time.Time
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

	// SendInvitation delivers an offer of membership. accountName is included
	// because the recipient may not be expecting it, and an unexplained link
	// asking for a password is indistinguishable from a phish.
	SendInvitation(ctx context.Context, to, token, accountName string) error
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
