package decisionlog

import (
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
)

// Module composes this feature's service and exposes it to the rest of the
// application. It is the single entry point anything outside this package is
// allowed to touch.
type Module struct {
	svc *Service
}

// New builds the module against a Postgres store.
func New(db *database.DB) *Module {
	return &Module{svc: NewService(NewPostgresRepository(db))}
}

// Service returns the module's service, for other modules to consume through
// their own ports.go interface.
func (m *Module) Service() *Service { return m.svc }
