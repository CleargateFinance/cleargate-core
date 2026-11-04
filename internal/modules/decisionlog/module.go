package decisionlog

import (
	"errors"
	"fmt"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
)

// Module composes this feature's service and exposes it to the rest of the
// application. It is the single entry point anything outside this package is
// allowed to touch.
type Module struct {
	svc *Service
	log *Log
}

// Deps are what this module needs from the composition root.
type Deps struct {
	DB *database.DB

	// Accounts is required. It decides who may read an agent's decisions,
	// and no stand-in has a safe answer: refusing everyone is a broken log,
	// and allowing everyone shows one customer's payments to another.
	Accounts Accounts

	// Payments is required: a decision shown without what it was about is
	// not a log anyone can act on.
	Payments Payments
}

// ErrMissingDependency reports a module built without something it cannot
// safely invent for itself.
var ErrMissingDependency = errors.New("decisionlog: missing required dependency")

// New builds the module against Postgres. Something that only writes
// decisions or keeps partitions, as the worker does, builds a Service with
// NewService instead and wires neither port.
func New(d Deps) (*Module, error) {
	switch {
	case d.DB == nil:
		return nil, fmt.Errorf("%w: database", ErrMissingDependency)
	case d.Accounts == nil:
		return nil, fmt.Errorf("%w: accounts", ErrMissingDependency)
	case d.Payments == nil:
		return nil, fmt.Errorf("%w: payments", ErrMissingDependency)
	}
	repo := NewPostgresRepository(d.DB)
	return &Module{svc: NewService(repo), log: NewLog(repo, d.Accounts, d.Payments)}, nil
}

// Service returns the module's service, for other modules to consume through
// their own ports.go interface.
func (m *Module) Service() *Service { return m.svc }

// Log returns the console's read of the log.
func (m *Module) Log() *Log { return m.log }
