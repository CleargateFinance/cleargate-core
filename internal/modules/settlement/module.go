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

	// Accounts decides who may read an account's deposit address. Required:
	// the route has no safe answer without it.
	Accounts Accounts

	// Rail builds and signs payments. Required: there is no safe stand-in for
	// the thing that moves money.
	Rail Rail

	// Token is the money on this rail, reported beside the address. Required:
	// a deposit in the wrong token is never credited.
	Token Token
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
	case d.Accounts == nil:
		return nil, fmt.Errorf("%w: accounts", ErrMissingDependency)
	case d.Rail == nil:
		return nil, fmt.Errorf("%w: rail", ErrMissingDependency)
	case d.Token.Symbol == "" || d.Token.Network == "" || d.Token.Mint == "" || d.Token.Decimals <= 0:
		return nil, fmt.Errorf("%w: token", ErrMissingDependency)
	}
	svc := NewService(NewPostgresRepository(d.DB), d.Wallets)
	svc.accounts, svc.rail, svc.token = d.Accounts, d.Rail, d.Token
	return &Module{svc: svc}, nil
}

// Service returns the module's service, for other modules to consume through
// their own ports.go interface.
func (m *Module) Service() *Service { return m.svc }
