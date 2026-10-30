package mandate

import (
	"errors"
	"fmt"
	"log/slog"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
)

// Module composes this feature's service and exposes it to the rest of the
// application. It is the single entry point anything outside this package is
// allowed to touch.
type Module struct {
	svc *Service
}

// Deps are what this module needs from the composition root.
type Deps struct {
	DB    *database.DB
	Clock Clock
	Log   *slog.Logger

	// Accounts is required. It is what decides who may grant authority, and
	// there is no safe default for that question: any stand-in either refuses
	// everyone, which is a broken module, or allows everyone, which is worse.
	Accounts Accounts
}

// ErrMissingDependency reports a module built without something it cannot
// safely invent for itself.
var ErrMissingDependency = errors.New("mandate: missing required dependency")

// New builds the module against Postgres.
func New(d Deps) (*Module, error) {
	if d.Accounts == nil {
		return nil, fmt.Errorf("%w: accounts", ErrMissingDependency)
	}
	repo := NewPostgresRepository(d.DB)
	return &Module{svc: NewService(repo, d.Accounts, d.Clock, d.Log)}, nil
}

// Service returns the module's service, for other modules to consume through
// the interface they declare in their own ports.
func (m *Module) Service() *Service { return m.svc }
