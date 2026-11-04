package summary

// Interfaces this module needs from the outside world, declared by the
// consumer (this module) rather than the provider. The module has no storage
// of its own, so these are all it depends on.

import (
	"context"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Accounts is what this module needs from the account module, which decides
// who may see an account at all.
type Accounts interface {
	// Membership returns the account as the actor belongs to it, reporting
	// the account module's not-permitted error when they do not.
	Membership(ctx context.Context, actor Actor, accountID id.AccountID) (Account, error)

	// Agents lists the account's agents, revoked ones included.
	Agents(ctx context.Context, actor Actor, accountID id.AccountID) ([]Agent, error)
}

// Ledger is what this module needs from the ledger module.
type Ledger interface {
	// Balance is the account's customer funds in the unit of account.
	Balance(ctx context.Context, accountID id.AccountID) (currency.Amount, error)

	// Spent is what an agent has spent in the day and month windows that
	// contain at, from the counters the caps are checked against.
	Spent(ctx context.Context, agentID id.AgentID, at time.Time) (day, month currency.Amount, err error)

	// Windows returns where the day and month windows containing at begin.
	// The ledger defines them, since its counters are kept in them, and a
	// second definition here could drift from the one the caps use.
	Windows(at time.Time) (dayStart, monthStart time.Time)
}

// Mandates is what this module needs from the mandate module.
type Mandates interface {
	// Current returns the caps of the agent's mandate in force, or nil when
	// it has none.
	Current(ctx context.Context, agentID id.AgentID) (*Caps, error)
}

// Payments is what this module needs from the payment module.
type Payments interface {
	// DeclinesSince totals the account's declines decided at or after since,
	// keyed by agent.
	DeclinesSince(ctx context.Context, accountID id.AccountID, since time.Time) (map[id.AgentID]Declines, error)
}

// Clock supplies the current time, which picks the windows the summary is in.
type Clock interface {
	Now() time.Time
}
