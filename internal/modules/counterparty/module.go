package counterparty

import (
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
)

// Module composes this feature's service — the single entry point the rest
// of the application is allowed to touch. The directory's routes are not in
// the MVP (Task 5.5).
type Module struct {
	svc *Service
}

// New builds the module against Postgres. Passing a nil clock selects the
// real one.
func New(db *database.DB, clock Clock) *Module {
	return &Module{svc: NewService(NewPostgresRepository(db), clock)}
}

// Service returns the module's service, for other modules to consume through
// their own ports.go interface.
func (m *Module) Service() *Service { return m.svc }
