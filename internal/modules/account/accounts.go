package account

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Membership is one account a person belongs to, as the console lists
// it: which account, what they may do in it, and whether it is frozen.
type Membership struct {
	AccountID id.AccountID
	Name      string
	Role      PermissionRole

	// FrozenAt is set while the account's owners have stopped all spending.
	// A platform suspension is a different field and is not shown here.
	FrozenAt *time.Time
}

// Accounts lists the accounts a person belongs to.
//
// It is how the console learns an account's identifier at all: sign-up
// creates the account, and every account route names one.
func (s *Service) Accounts(ctx context.Context, actor Session) ([]Membership, error) {
	return s.repo.AccountsOf(ctx, actor.UserID)
}

// Agents lists an account's agents, revoked ones included, so the console can
// show whose decision history is whose.
//
// Any member may read it. A viewer reads the console; it is changing an agent
// that needs a manager's role.
func (s *Service) Agents(ctx context.Context, actor Session, accountID id.AccountID) ([]Agent, error) {
	if _, err := s.repo.MembershipRole(ctx, accountID, actor.UserID); err != nil {
		return nil, err
	}
	return s.repo.AgentsOf(ctx, accountID)
}

// RequireAgentReader reports whether a session may read an agent's records,
// its decision log above all.
//
// Any member of the agent's account may, by the reasoning in Agents, and a
// revoked agent stays readable. Somebody outside the account is told the
// agent does not exist, as requireAgentManagerOf explains.
func (s *Service) RequireAgentReader(ctx context.Context, actor Session, agentID id.AgentID) error {
	agent, err := s.repo.Agent(ctx, agentID)
	if err != nil {
		return err
	}
	_, err = s.repo.MembershipRole(ctx, agent.AccountID, actor.UserID)
	if errors.Is(err, ErrNotPermitted) {
		return ErrAgentNotFound
	}
	return err
}

// FreezeAccount stops every agent on the account from spending, at once.
//
// The widest of the three revocations (AgentCredential): what an owner
// reaches for when something is wrong and they do not yet know which agent it
// is. A payment already committing finishes first and every later one is
// declined, because the payment path holds the account row (HoldSpending).
//
// Owner, admin or guardian, and not gated on a second factor: freezing only
// removes authority (Task 2.8's risk-reducing class), and the moment it is
// needed is when the person acting may be nowhere near their codes. Freezing
// a frozen account changes nothing, so whoever froze it first stays recorded.
func (s *Service) FreezeAccount(ctx context.Context, actor Session, accountID id.AccountID) error {
	role, err := s.repo.MembershipRole(ctx, accountID, actor.UserID)
	if err != nil {
		return err
	}
	if role != RoleOwner && role != RoleAdmin && role != RoleGuardian {
		return ErrNotPermitted
	}

	frozen, err := s.repo.FreezeAccount(ctx, accountID, actor.UserID, s.clock.Now())
	if err != nil {
		return err
	}
	if frozen {
		s.log.Warn("account frozen",
			slog.String("account_id", string(accountID)),
			slog.String("user_id", string(actor.UserID)),
		)
	}
	return nil
}

// UnfreezeAccount lets the account's agents spend again, each within its
// mandate. It does not lift a platform suspension, which is a separate field.
//
// Owner or admin, behind a recent step-up. Unlike freezing, this restores
// authority to every agent at once, and an account is usually frozen because
// somebody suspects a compromise: if a stolen session were enough to undo it,
// the freeze would last only until the attacker noticed. A guardian may
// freeze but not unfreeze, since its power is subtractive only.
//
// Unfreezing is instant for any owner or admin. Delaying it for everyone but
// whoever froze it is Task 2.8, and not in the MVP.
func (s *Service) UnfreezeAccount(ctx context.Context, actor Session, accountID id.AccountID) error {
	if err := s.requireAgentManager(ctx, actor, accountID); err != nil {
		return err
	}
	if err := s.RequireStepUp(ctx, actor); err != nil {
		return err
	}

	unfrozen, err := s.repo.UnfreezeAccount(ctx, accountID)
	if err != nil {
		return err
	}
	if unfrozen {
		s.log.Warn("account unfrozen",
			slog.String("account_id", string(accountID)),
			slog.String("user_id", string(actor.UserID)),
		)
	}
	return nil
}
