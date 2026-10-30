package mandate

import (
	"context"
	"log/slog"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Service implements this module's use cases. It depends on the interfaces in
// ports.go, never on the storage implementation directly.
type Service struct {
	repo     Repository
	accounts Accounts
	clock    Clock
	log      *slog.Logger
}

// systemClock is the default time source, used when none is injected.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// NewService builds a Service. A nil clock selects the real one and a nil
// logger the default.
func NewService(repo Repository, accounts Accounts, clock Clock, log *slog.Logger) *Service {
	if clock == nil {
		clock = systemClock{}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Service{repo: repo, accounts: accounts, clock: clock, log: log}
}

// Issue grants an agent new spending authority, replacing whatever it held.
//
// The new version is in force from now. There is no timelock and no veto yet
// (Task 2.8), so the step-up is the whole of the approval: the account module
// refuses unless the actor manages mandates in the agent's account and has
// entered a second factor recently.
//
// Loosening and tightening are treated alike for the same reason. Once a
// timelock exists, only loosening waits, since reducing what an agent may
// spend never needs a waiting period.
func (s *Service) Issue(ctx context.Context, actor Actor, agentID id.AgentID, rules Rules) (Mandate, error) {
	now := s.clock.Now()

	rules = rules.normalised()
	if err := rules.Validate(now); err != nil {
		return Mandate{}, err
	}

	agent, err := s.accounts.Agent(ctx, agentID)
	if err != nil {
		return Mandate{}, err
	}
	if !agent.Active {
		return Mandate{}, ErrAgentRevoked
	}
	if err := s.accounts.RequireMandateIssuer(ctx, actor, agent.AccountID); err != nil {
		return Mandate{}, err
	}

	stored, err := s.repo.IssueVersion(ctx, agentID, now, func(version int) (Draft, error) {
		payload, err := Canonicalise(agentID, version, rules)
		if err != nil {
			return Draft{}, err
		}
		return Draft{
			AgentID:       agentID,
			Version:       version,
			Payload:       payload,
			Scheme:        SchemeSessionStepUp,
			SignedBy:      actor.UserID,
			Status:        StatusActive,
			EffectiveFrom: now,
		}, nil
	})
	if err != nil {
		return Mandate{}, err
	}

	m, err := s.verified(stored)
	if err != nil {
		return Mandate{}, err
	}

	s.log.Info("mandate issued",
		slog.String("account_id", string(agent.AccountID)),
		slog.String("agent_id", string(agentID)),
		slog.String("mandate_id", string(m.ID)),
		slog.Int("version", m.Version),
	)

	return m, nil
}

// Current returns the mandate in force for an agent now, which is what the
// authorization path evaluates a payment against.
//
// It takes no actor. The caller is the payment path acting for an agent that
// has already authenticated, and an agent may always learn its own limits; it
// is never a console route, where a human's role would have to be checked.
func (s *Service) Current(ctx context.Context, agentID id.AgentID) (Mandate, error) {
	m, err := s.repo.Active(ctx, agentID)
	if err != nil {
		return Mandate{}, err
	}
	return s.verified(m)
}

// At returns the mandate that was in force for an agent at an instant: the
// question a dispute asks about a payment made then.
func (s *Service) At(ctx context.Context, agentID id.AgentID, at time.Time) (Mandate, error) {
	m, err := s.repo.ActiveAt(ctx, agentID, at)
	if err != nil {
		return Mandate{}, err
	}
	return s.verified(m)
}

// verified parses a stored mandate's rules from its payload, and refuses one
// whose approval this build cannot check.
//
// The rules come from the bytes and nowhere else, never from the projection
// columns, which exist for queries and are only as trustworthy as the trigger
// that fills them.
//
// A 'webauthn' mandate is refused outright until Task 3.1 ships a verifier.
// None can be issued yet, so one appearing means a row was written by
// something other than this service, and trusting its rules because the
// scheme sounds strong would turn a forged row into spending authority.
//
// A 'session_stepup' mandate has nothing to verify. It records that a signed-in
// person approved it and carries no signature, so a write to this table that
// bypasses the service is not detectable here. That is the gap Task 3.1 closes.
func (s *Service) verified(m Mandate) (Mandate, error) {
	if m.Scheme != SchemeSessionStepUp {
		s.log.Error("mandate with unverifiable approval refused",
			slog.String("mandate_id", string(m.ID)),
			slog.String("scheme", string(m.Scheme)),
		)
		return Mandate{}, ErrUnverifiable
	}

	agentID, version, rules, err := parsePayload(m.Payload)
	if err != nil {
		return Mandate{}, apperr.Internal("mandate: stored payload is unreadable", err)
	}
	if agentID != m.AgentID || version != m.Version {
		// The database refuses this on insert, so reaching it means the row
		// was not written through the table as it is defined today.
		return Mandate{}, apperr.Internal("mandate: payload does not match its row", nil)
	}

	m.Rules = rules
	return m, nil
}
