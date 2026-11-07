package account

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/cache"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Module composes this feature's service and exposes it to the rest of the
// application. It is the single entry point anything outside this package is
// allowed to touch.
type Module struct {
	svc *Service
}

// Deps are what this module needs from the composition root.
type Deps struct {
	DB     *database.DB
	Cache  *cache.Client
	Hasher *crypto.Hasher
	Clock  Clock
	Log    *slog.Logger

	// Mailer is required. There is deliberately no default: the only
	// implementation that needs no configuration is the one that writes live
	// credentials to the log, and nothing should get that by omission.
	Mailer Mailer

	// Sealer is required, for the same reason. The only implementation that
	// needs no configuration is one with a key compiled into the binary, and a
	// hardcoded key would make every stored second factor recoverable by
	// anybody holding a copy of the source.
	Sealer SecretSealer

	Options Options
}

// ErrMissingDependency reports a module built without something it cannot
// safely invent for itself.
var ErrMissingDependency = errors.New("account: missing required dependency")

// New builds the module against Postgres and Redis.
//
// A nil hasher selects the real cost parameters, so production wiring cannot
// silently inherit the cheap ones a test configured. The mailer and the sealer
// have no such fallback and are refused when absent, because in both cases the
// only zero-configuration implementation is an unsafe one.
func New(d Deps) (*Module, error) {
	if d.Mailer == nil {
		return nil, fmt.Errorf("%w: mailer", ErrMissingDependency)
	}
	if d.Sealer == nil {
		return nil, fmt.Errorf("%w: sealer", ErrMissingDependency)
	}

	hasher := d.Hasher
	if hasher == nil {
		hasher = crypto.NewHasher(crypto.DefaultParams())
	}

	repo := NewPostgresRepository(d.DB)
	throttle := NewCacheThrottle(d.Cache, d.Cache, d.Log)

	return &Module{
		svc: NewService(repo, hasher, d.Cache, d.Mailer, d.Sealer,
			throttle, d.Clock, d.Log, d.Options),
	}, nil
}

// Principals reads who owns an account, without the service's login machinery.
// A cap is per principal while a deposit names an account, so the funding path
// needs this in both directions.
type Principals struct {
	repo *postgresRepo
}

// NewPrincipals builds a Principals against Postgres.
func NewPrincipals(db *database.DB) *Principals {
	return &Principals{repo: &postgresRepo{db: db}}
}

// Of returns the principal an account belongs to.
func (p *Principals) Of(ctx context.Context, accountID id.AccountID) (id.PrincipalID, error) {
	return p.repo.principalOf(ctx, accountID)
}

// Accounts returns every account a principal holds.
func (p *Principals) Accounts(ctx context.Context, principalID id.PrincipalID) ([]id.AccountID, error) {
	return p.repo.accountsOfPrincipal(ctx, principalID)
}

// NewFundingGate builds a service for the worker's funding path. It holds no
// mailer, sealer or hasher, so calling anything but RequireFundingAllowed and
// RecordFunding on it panics rather than signing somebody in. history is
// required: RequireFundingAllowed refuses without one.
func NewFundingGate(db *database.DB, history FundingHistory, log *slog.Logger) *Service {
	return NewService(NewPostgresRepository(db), nil, nil, nil, nil, nil, nil, log,
		Options{FundingHistory: history})
}

// Service returns the module's service, for other modules to consume through
// the interface they declare in their own ports.
func (m *Module) Service() *Service { return m.svc }
