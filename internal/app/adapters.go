package app

import (
	"context"

	"github.com/CleargateFinance/cleargate-core/internal/modules/account"
	"github.com/CleargateFinance/cleargate-core/internal/modules/mandate"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Adapters translate one module's service into the port another declares.
// They live here because this is the only package allowed to know both sides,
// and they hold no logic of their own beyond the translation.

// MandateAccounts satisfies mandate.Accounts with the account module. Wired
// into BuildAPI with the console's mandate routes (Task 6.2).
type MandateAccounts struct {
	Accounts *account.Service
}

var _ mandate.Accounts = MandateAccounts{}

// Agent implements mandate.Accounts.
func (a MandateAccounts) Agent(ctx context.Context, agentID id.AgentID) (mandate.Agent, error) {
	agent, err := a.Accounts.Agent(ctx, agentID)
	if err != nil {
		return mandate.Agent{}, err
	}
	return mandate.Agent{ID: agent.ID, AccountID: agent.AccountID, Active: agent.Active()}, nil
}

// RequireMandateIssuer implements mandate.Accounts.
//
// The session is rebuilt from the two fields the actor carries, which are the
// only ones the role and step-up checks read. The handler copies them from a
// session account.ValidateSession returned on the same request.
func (a MandateAccounts) RequireMandateIssuer(ctx context.Context, actor mandate.Actor, accountID id.AccountID) error {
	return a.Accounts.RequireMandateIssuer(ctx,
		account.Session{UserID: actor.UserID, MFASatisfiedAt: actor.MFASatisfiedAt},
		accountID)
}
