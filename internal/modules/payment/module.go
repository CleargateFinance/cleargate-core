package payment

import (
	"errors"
	"fmt"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
)

// Module composes this feature's service — the single entry point the rest
// of the application is allowed to touch.
type Module struct {
	svc *Service
}

// Deps are what this module needs from the composition root. Every port is
// required: each one answers a question the decision depends on, and no
// stand-in has a safe answer to "may this agent spend?".
type Deps struct {
	DB    *database.DB
	Clock Clock

	Accounts       Accounts
	Mandates       Mandates
	Counterparties Counterparties
	Ledger         Ledger
	Decisions      DecisionLog
	Outbox         Outbox

	// Transfers quotes and signs. Required: an approved payment that cannot be
	// signed is money debited for nothing.
	Transfers Transfers

	// Rail is recorded on every payment. Required, since a payment row with
	// no rail cannot be reconciled with the chain.
	Rail Rail
}

// ErrMissingDependency reports a module built without something it cannot
// safely invent for itself.
var ErrMissingDependency = errors.New("payment: missing required dependency")

// New builds the module against Postgres. A nil clock selects the real one.
func New(d Deps) (*Module, error) {
	switch {
	case d.DB == nil:
		return nil, fmt.Errorf("%w: database", ErrMissingDependency)
	case d.Accounts == nil:
		return nil, fmt.Errorf("%w: accounts", ErrMissingDependency)
	case d.Mandates == nil:
		return nil, fmt.Errorf("%w: mandates", ErrMissingDependency)
	case d.Counterparties == nil:
		return nil, fmt.Errorf("%w: counterparties", ErrMissingDependency)
	case d.Ledger == nil:
		return nil, fmt.Errorf("%w: ledger", ErrMissingDependency)
	case d.Decisions == nil:
		return nil, fmt.Errorf("%w: decision log", ErrMissingDependency)
	case d.Outbox == nil:
		return nil, fmt.Errorf("%w: outbox", ErrMissingDependency)
	case d.Transfers == nil:
		return nil, fmt.Errorf("%w: transfers", ErrMissingDependency)
	case d.Rail.Name == "" || d.Rail.Token == "" || d.Rail.Network == "":
		return nil, fmt.Errorf("%w: rail", ErrMissingDependency)
	}

	return &Module{svc: &Service{
		repo:      NewPostgresRepository(d.DB),
		uow:       d.DB.UoW,
		accounts:  d.Accounts,
		mandates:  d.Mandates,
		payees:    d.Counterparties,
		ledger:    d.Ledger,
		decisions: d.Decisions,
		events:    d.Outbox,
		clock:     clockOrSystem(d.Clock),
		rail:      d.Rail,
		transfers: d.Transfers,
	}}, nil
}

// Service returns the module's service, for other modules to consume through
// their own ports.go interface.
func (m *Module) Service() *Service { return m.svc }

// NewRetention builds the key expiry the worker runs. A nil clock selects the
// real one.
func NewRetention(db *database.DB, clock Clock) *Retention {
	return &Retention{repo: NewPostgresRepository(db), clock: clockOrSystem(clock)}
}

// NewLookup builds the read of recorded payments the decision log uses.
func NewLookup(db *database.DB) *Lookup {
	return &Lookup{repo: NewPostgresRepository(db)}
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func clockOrSystem(c Clock) Clock {
	if c == nil {
		return systemClock{}
	}
	return c
}
