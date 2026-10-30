package mandate

import (
	"context"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Repository is the storage this module needs.
//
// Declared here, by the consumer, so Service depends only on this shape and
// the Postgres implementation can be replaced without service code changing.
type Repository interface {
	// IssueVersion stores an agent's next mandate as the one in force, in one
	// transaction: it reads the latest version, calls build with the number
	// the new mandate will carry, supersedes the active mandate as of at, and
	// inserts the draft.
	//
	// build is a callback because the version is inside the payload, and the
	// version is only known inside the transaction. A second issue racing this
	// one reports ErrConcurrentChange, and nothing is written.
	IssueVersion(ctx context.Context, agentID id.AgentID, at time.Time, build func(version int) (Draft, error)) (Mandate, error)

	// Active returns the agent's mandate in force now, reporting ErrNoMandate
	// when there is none.
	Active(ctx context.Context, agentID id.AgentID) (Mandate, error)

	// ActiveAt returns the mandate that was in force at an instant, reporting
	// ErrNoMandate when none was. This is the dispute query: which authority
	// applied to a payment made then, whatever has happened since.
	ActiveAt(ctx context.Context, agentID id.AgentID, at time.Time) (Mandate, error)
}

// Accounts is what this module needs from the account module.
//
// A port rather than a query of agent or membership tables from this module's
// repository, because those tables belong to the account module, and who may
// grant authority is its policy to state, in one place.
type Accounts interface {
	// Agent returns the agent a mandate would be issued to, reporting the
	// account module's not-found error when there is none.
	Agent(ctx context.Context, agentID id.AgentID) (Agent, error)

	// RequireMandateIssuer refuses unless the actor may grant spending
	// authority in this account right now: they hold a role that manages
	// mandates, and they have stepped up recently.
	RequireMandateIssuer(ctx context.Context, actor Actor, accountID id.AccountID) error
}

// Clock supplies the current time, so effective dates and expiry are testable
// without waiting for real time to pass.
type Clock interface {
	Now() time.Time
}
