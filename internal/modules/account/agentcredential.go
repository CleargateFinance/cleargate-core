package account

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// ConnectAgent registers an agent against an account.
//
// Not gated on a second factor, because this grants nothing. The row is a name
// and an identity: it cannot authenticate until a credential is issued, and it
// cannot spend until a mandate says so. Gating it would put a prompt in front
// of an action with no authority attached.
func (s *Service) ConnectAgent(ctx context.Context, actor Session, accountID id.AccountID, name string) (Agent, error) {
	if err := ValidateAgentName(name); err != nil {
		return Agent{}, err
	}
	if err := s.requireAgentManager(ctx, actor, accountID); err != nil {
		return Agent{}, err
	}

	agent, err := s.repo.CreateAgent(ctx, accountID, strings.TrimSpace(name), actor.UserID)
	if err != nil {
		return Agent{}, err
	}

	s.log.Info("agent connected",
		slog.String("account_id", string(accountID)),
		slog.String("agent_id", string(agent.ID)),
	)

	return agent, nil
}

// IssueCredential mints a key for an agent and returns it exactly once.
//
// Gated on a second factor, unlike connecting the agent itself. The difference
// is what the two produce: this one mints a bearer secret that keeps working
// long after the session that created it is gone, so a stolen cookie alone must
// not be enough to plant one.
//
// The returned key is the only copy that will ever exist outside the caller.
// Only its digest is stored, so a disclosure of the credential table yields
// nothing presentable and a lost key is rotated rather than looked up.
func (s *Service) IssueCredential(ctx context.Context, actor Session, agentID id.AgentID, name string) (crypto.APIKey, AgentCredential, error) {
	if err := ValidateAgentName(name); err != nil {
		return crypto.APIKey{}, AgentCredential{}, err
	}

	agent, err := s.repo.Agent(ctx, agentID)
	if err != nil {
		return crypto.APIKey{}, AgentCredential{}, err
	}
	if !agent.Active() {
		return crypto.APIKey{}, AgentCredential{}, ErrAgentRevoked
	}

	if err := s.requireAgentManager(ctx, actor, agent.AccountID); err != nil {
		return crypto.APIKey{}, AgentCredential{}, err
	}
	if err := s.RequireStepUp(ctx, actor); err != nil {
		return crypto.APIKey{}, AgentCredential{}, err
	}

	key, err := crypto.NewAPIKey()
	if err != nil {
		return crypto.APIKey{}, AgentCredential{}, err
	}

	credential, err := s.repo.CreateAgentCredential(ctx, agentID, strings.TrimSpace(name),
		key.Prefix(), key.Digest(), actor.UserID)
	if err != nil {
		return crypto.APIKey{}, AgentCredential{}, err
	}

	// The prefix is logged and the secret is not. That is the whole point of
	// splitting them: an operator can tell which credential this was.
	s.log.Info("agent credential issued",
		slog.String("agent_id", string(agentID)),
		slog.String("prefix", key.Prefix()),
	)

	return key, credential, nil
}

// RevokeCredential retires one key.
//
// Not gated on a second factor. Revoking only ever removes access, and the
// moment it matters most is a compromise, when whoever needs to act may be
// nowhere near the device that holds their codes.
func (s *Service) RevokeCredential(ctx context.Context, actor Session, agentID id.AgentID, credentialID string) error {
	agent, err := s.repo.Agent(ctx, agentID)
	if err != nil {
		return err
	}
	if err := s.requireAgentManager(ctx, actor, agent.AccountID); err != nil {
		return err
	}

	revoked, err := s.repo.RevokeAgentCredential(ctx, agentID, credentialID, s.clock.Now())
	if err != nil {
		return err
	}
	if !revoked {
		return ErrCredentialNotFound
	}

	s.log.Info("agent credential revoked",
		slog.String("agent_id", string(agentID)),
		slog.String("credential_id", credentialID),
	)

	return nil
}

// RevokeAgent retires an agent and every key it holds, in one transaction.
//
// The wider of the two revocations. Reach for it when the agent itself is the
// problem rather than one deployed key, and note that it is final: the identity
// keeps its decision history, and reconnecting is a new registration so that
// history stays attributable to whoever made it.
func (s *Service) RevokeAgent(ctx context.Context, actor Session, agentID id.AgentID) error {
	agent, err := s.repo.Agent(ctx, agentID)
	if err != nil {
		return err
	}
	if err := s.requireAgentManager(ctx, actor, agent.AccountID); err != nil {
		return err
	}
	if !agent.Active() {
		// Already gone. Revoking twice is not a failure worth reporting, and a
		// caller containing an incident should not have to care.
		return nil
	}

	if err := s.repo.RevokeAgent(ctx, agentID, s.clock.Now()); err != nil {
		return err
	}

	s.log.Warn("agent revoked",
		slog.String("account_id", string(agent.AccountID)),
		slog.String("agent_id", string(agentID)),
	)

	return nil
}

// AuthenticateAgent resolves a presented key to the identity behind it.
//
// This runs on every request an agent makes, so it is one indexed lookup by the
// non-secret prefix followed by a constant-time comparison of the secret. Every
// condition that decides whether the credential may authenticate lives in the
// query, so a caller cannot hold an identity that looks usable but is not.
//
// What it returns carries no authority. Spending is decided per payment against
// a mandate, so a valid credential on its own moves nothing.
func (s *Service) AuthenticateAgent(ctx context.Context, key string) (AgentIdentity, error) {
	prefix, secret, err := crypto.ParseAPIKey(key)
	if err != nil {
		// A key that is not even the right shape never reaches the database.
		// This is also what stops a human session token being presented here
		// and matched against anything.
		return AgentIdentity{}, ErrCredentialInvalid
	}

	identity, digest, err := s.repo.ActiveCredentialByPrefix(ctx, prefix)
	if err != nil {
		if errors.Is(err, ErrCredentialInvalid) {
			return AgentIdentity{}, ErrCredentialInvalid
		}
		return AgentIdentity{}, err
	}

	if !crypto.SecretMatches(digest, secret) {
		// Identical to an unknown prefix, on purpose. Whoever presented this
		// learns only that it did not work.
		return AgentIdentity{}, ErrCredentialInvalid
	}

	// Advisory, so "which of my keys is still in use" can be answered before a
	// rotation retires one. A failure here must never fail a request that was
	// otherwise authenticated.
	if err := s.repo.TouchAgentCredential(ctx, identity.CredentialID, s.clock.Now()); err != nil {
		s.log.Warn("recording agent credential use failed",
			slog.String("error", err.Error()))
	}

	return identity, nil
}

// ListCredentials returns an agent's keys, without any secret.
//
// What a rotation needs to be safe: which keys exist, which are still live, and
// when each was last used, so the one being retired can be confirmed idle
// before it is switched off.
func (s *Service) ListCredentials(ctx context.Context, actor Session, agentID id.AgentID) ([]AgentCredential, error) {
	agent, err := s.repo.Agent(ctx, agentID)
	if err != nil {
		return nil, err
	}
	if err := s.requireAgentManager(ctx, actor, agent.AccountID); err != nil {
		return nil, err
	}

	return s.repo.AgentCredentials(ctx, agentID)
}
