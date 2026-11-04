package decisionlog

import (
	"context"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Repository is what this module needs from storage.
type Repository interface {
	// Insert stores a decision and its checks, joining the transaction open on
	// ctx. The caller's transaction is what makes the decision commit with
	// the payment it judged, or not at all.
	Insert(ctx context.Context, r Record) (id.DecisionID, error)

	// EnsurePartitions creates any missing monthly partitions of the decision
	// tables, covering monthsAhead months from now.
	EnsurePartitions(ctx context.Context, monthsAhead int) error

	// AgentDecisions returns up to q.Limit of the agent's decisions, newest
	// first, with their checks in the order they ran. Payment is left zero;
	// it is another module's to fill.
	AgentDecisions(ctx context.Context, agentID id.AgentID, q Query) ([]Entry, error)
}

// Accounts is what the log needs from the account module.
//
// A port rather than a query of agent or membership tables here, because
// those belong to the account module, and who may see an agent's records is
// its policy to state, in one place.
type Accounts interface {
	// RequireAgentReader refuses unless the actor belongs to the account the
	// agent is in. An unknown agent and somebody else's are one answer, the
	// account module's not-found.
	RequireAgentReader(ctx context.Context, actor Actor, agentID id.AgentID) error
}

// Payments is what the log needs from the payment module: what each decision
// was about.
type Payments interface {
	// AgentPayments returns the agent's payments among ids, keyed by id. An id
	// that is not one of this agent's payments is absent from the map.
	AgentPayments(ctx context.Context, agentID id.AgentID, ids []id.PaymentID) (map[id.PaymentID]Payment, error)
}
