package settlement

import (
	"errors"
	"fmt"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
)

// Module composes this feature's service — the single entry point the rest
// of the application is allowed to touch.
type Module struct {
	svc *Service
}

// Deps are what this module needs from the composition root.
type Deps struct {
	DB *database.DB

	// Wallets is required. There is no safe stand-in for a custodian: a fake
	// one hands out addresses nobody holds the key to, and every deposit to
	// them is lost.
	Wallets WalletProvider
}

// ErrMissingDependency reports a module built without something it cannot
// safely invent for itself.
var ErrMissingDependency = errors.New("settlement: missing required dependency")

// New builds the module against Postgres.
func New(d Deps) (*Module, error) {
	switch {
	case d.DB == nil:
		return nil, fmt.Errorf("%w: database", ErrMissingDependency)
	case d.Wallets == nil:
		return nil, fmt.Errorf("%w: wallet provider", ErrMissingDependency)
	}
	return &Module{svc: NewService(NewPostgresRepository(d.DB), d.Wallets)}, nil
}

// Service returns the module's service, for other modules to consume through
// their own ports.go interface.
func (m *Module) Service() *Service { return m.svc }
