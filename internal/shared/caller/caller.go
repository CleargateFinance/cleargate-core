// Package caller carries who is making a request, from the middleware that
// authenticated it to the handlers that act on it.
//
// It holds two kinds of caller and never one that is both. An agent is
// authorized by its mandate and a person by their role (§4.1), so a handler
// asks for the kind its surface serves, and a request authenticated as the
// other kind simply has none.
//
// It lives in shared, rather than in the account module that authenticates,
// so a handler in another module can read its caller without importing that
// module.
package caller

import (
	"context"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Agent is an agent authenticated by its API key. It carries no authority:
// what it may spend is decided per payment against its mandate.
type Agent struct {
	AgentID      id.AgentID
	AccountID    id.AccountID
	CredentialID string
}

// User is a person authenticated by their console session.
type User struct {
	UserID id.UserID

	// MFASatisfiedAt is when the session last entered a second factor, which
	// step-up actions require to be recent.
	MFASatisfiedAt *time.Time
}

type agentKey struct{}

type userKey struct{}

// WithAgent returns ctx carrying a.
func WithAgent(ctx context.Context, a Agent) context.Context {
	return context.WithValue(ctx, agentKey{}, a)
}

// AgentFrom returns the agent ctx carries, if it carries one.
func AgentFrom(ctx context.Context) (Agent, bool) {
	a, ok := ctx.Value(agentKey{}).(Agent)
	return a, ok
}

// WithUser returns ctx carrying u.
func WithUser(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, userKey{}, u)
}

// UserFrom returns the person ctx carries, if it carries one.
func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(userKey{}).(User)
	return u, ok
}
