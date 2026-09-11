package account

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// MaxAgentNameLength bounds a name a human types to tell one agent from
// another. Generous enough for a descriptive label, short enough that an
// authenticated endpoint cannot be handed a megabyte to store.
const MaxAgentNameLength = 128

// AgentStatus is whether an agent may act at all.
type AgentStatus string

const (
	// AgentActive can authenticate, and can spend if a mandate says so.
	AgentActive AgentStatus = "active"

	// AgentRevoked can do neither, and cannot be brought back. Reconnecting is
	// a new registration, so the decision history of the old one stays
	// attributable to the identity that made it.
	AgentRevoked AgentStatus = "revoked"
)

// Agent is a registration, not a program.
//
// The software runs on the customer's own infrastructure. This row is the
// identity it is connected as, and it is deliberately inert: a credential lets
// it authenticate and a mandate lets it spend, so connecting one on its own
// grants nothing at all.
type Agent struct {
	ID        id.AgentID
	AccountID id.AccountID

	Name      string
	Status    AgentStatus
	CreatedBy id.UserID
	CreatedAt time.Time
	RevokedAt *time.Time
}

// Active reports whether this agent may authenticate.
func (a Agent) Active() bool { return a.Status == AgentActive }

// AgentCredential is one key an agent authenticates with.
//
// Credentials are separate objects from the agent so a key can be rotated
// without destroying the identity that carries the mandate and the decision
// history. Issue the new one, deploy it, revoke the old one, and both work
// during the overlap, so nothing goes down to do it.
//
// It also gives revocation three widths: this one key, every key belonging to
// an agent, or every agent on the account. The third is what you reach for when
// you know something is wrong but not yet which agent it is.
type AgentCredential struct {
	ID      string
	AgentID id.AgentID

	// Name is which key to revoke at two in the morning, so it should say
	// where the key was deployed rather than what it is for.
	Name string

	// Prefix is the non-secret handle. It is the lookup key and it is safe to
	// log, which is what lets somebody identify a credential without handling
	// the secret.
	Prefix string

	CreatedBy  id.UserID
	CreatedAt  time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
}

// Active reports whether this credential may still authenticate.
func (c AgentCredential) Active() bool { return c.RevokedAt == nil }

// AgentIdentity is what a verified credential resolves to.
//
// It carries who is calling and nothing about what they may do. Spending
// authority comes from a mandate, which is looked up per payment, so there is
// no path from holding a credential to moving money.
type AgentIdentity struct {
	AgentID      id.AgentID
	AccountID    id.AccountID
	CredentialID string
	Prefix       string
}

// AgentSession groups an agent's payments into one run.
//
// It has a start and no end, deliberately. What makes a run useful is being
// able to say which payments belonged to the same piece of work, and that is
// established when the work begins. A closing timestamp would only record when
// something stopped calling, which for a crashed process never arrives.
//
// It cannot be reconstructed afterwards either, which is why it is opened
// explicitly rather than inferred from a gap between payments.
type AgentSession struct {
	ID      id.AgentSessionID
	AgentID id.AgentID

	// TaskRef is the caller's own identifier for the run, so a payment here can
	// be matched to a job in their system during a dispute.
	TaskRef string

	// TaskText is what the agent was asked to do, in words. It is the evidence
	// a human reads when deciding whether a payment should have happened.
	TaskText string

	StartedAt time.Time
}

// Errors reported by the agent flows.
var (
	// ErrAgentNotFound reports an agent that does not exist, or belongs to
	// another account. The two are one answer, so an identifier cannot be used
	// to probe what exists outside the caller's own account.
	ErrAgentNotFound = apperr.NotFound("agent not found")

	// ErrAgentRevoked reports an operation on an agent that has been retired.
	ErrAgentRevoked = apperr.Conflict("this agent has been revoked")

	// ErrCredentialNotFound reports a credential that does not exist or is not
	// this agent's, undivided for the same reason.
	ErrCredentialNotFound = apperr.NotFound("credential not found")

	// ErrCredentialInvalid covers every way a presented key fails: malformed,
	// unknown prefix, wrong secret, revoked, or belonging to a revoked agent.
	//
	// One answer for all of them. A caller can act no differently on one than
	// another, and distinguishing "revoked" from "wrong secret" would tell
	// somebody holding a leaked key whether it was ever real.
	ErrCredentialInvalid = apperr.Unauthorized("agent credential is not valid")
)

// ValidateAgentName reports whether a name may be stored.
//
// Length is counted in runes, so a name in a non-Latin script is not penalised
// for encoding to more bytes per character.
func ValidateAgentName(name string) error {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return errInvalid("agent name is required")
	}
	if utf8.RuneCountInString(trimmed) > MaxAgentNameLength {
		return errInvalid("agent name is too long")
	}
	return nil
}

// requireAgentManager checks that an actor may manage agents in this account.
//
// Owners and admins only. Managing agents is the substance of the admin role,
// while a guardian's power is deliberately subtractive and a viewer changes
// nothing.
func (s *Service) requireAgentManager(ctx context.Context, actor Session, accountID id.AccountID) error {
	role, err := s.repo.MembershipRole(ctx, accountID, actor.UserID)
	if err != nil {
		return err
	}
	if role != RoleOwner && role != RoleAdmin {
		return ErrNotPermitted
	}
	return nil
}
