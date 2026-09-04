package ledger

import (
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
)

// Module composes this feature's service and exposes it to the rest of the
// application. It is the single entry point anything outside this package is
// allowed to touch.
type Module struct {
	svc *Service
}

// New builds the module against a Postgres store. Passing a nil clock selects
// the real one.
func New(db *database.DB, clock Clock) *Module {
	repo := NewPostgresRepository(db)
	return &Module{svc: NewService(repo, clock)}
}

// Service returns the module's service, for other modules to consume through
// the interface they declare in their own ports.
func (m *Module) Service() *Service { return m.svc }
