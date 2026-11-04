package summary

import (
	"errors"
	"fmt"
	"time"
)

// Module composes this feature's service and exposes it to the rest of the
// application. It is the single entry point anything outside this package is
// allowed to touch.
type Module struct {
	svc *Service
}

// Deps are what this module needs from the composition root.
type Deps struct {
	// Accounts is required. It decides who may see an account, and no
	// stand-in has a safe answer: refusing everyone is a broken page, and
	// allowing everyone shows one customer's balance to another.
	Accounts Accounts

	// The other three are required because each is a figure on the page, and
	// a summary that silently showed zero for one would be worse than none.
	Ledger   Ledger
	Mandates Mandates
	Payments Payments

	// Clock may be nil, which selects the real one.
	Clock Clock
}

// ErrMissingDependency reports a module built without something it cannot
// safely invent for itself.
var ErrMissingDependency = errors.New("summary: missing required dependency")

// New builds the module.
func New(d Deps) (*Module, error) {
	switch {
	case d.Accounts == nil:
		return nil, fmt.Errorf("%w: accounts", ErrMissingDependency)
	case d.Ledger == nil:
		return nil, fmt.Errorf("%w: ledger", ErrMissingDependency)
	case d.Mandates == nil:
		return nil, fmt.Errorf("%w: mandates", ErrMissingDependency)
	case d.Payments == nil:
		return nil, fmt.Errorf("%w: payments", ErrMissingDependency)
	}
	clock := d.Clock
	if clock == nil {
		clock = systemClock{}
	}
	return &Module{svc: NewService(d.Accounts, d.Ledger, d.Mandates, d.Payments, clock)}, nil
}

// Service returns the module's service.
func (m *Module) Service() *Service { return m.svc }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }
