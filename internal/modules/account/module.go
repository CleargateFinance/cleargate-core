package account

import (
	"log/slog"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/cache"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/crypto"
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
	DB     *database.DB
	Cache  *cache.Client
	Hasher *crypto.Hasher
	Clock  Clock
	Log    *slog.Logger

	Options Options
}

// New builds the module against Postgres and Redis.
//
// A nil hasher selects the real cost parameters, so production wiring cannot
// silently inherit the cheap ones a test configured.
func New(d Deps) *Module {
	hasher := d.Hasher
	if hasher == nil {
		hasher = crypto.NewHasher(crypto.DefaultParams())
	}

	repo := NewPostgresRepository(d.DB)

	return &Module{
		svc: NewService(repo, hasher, d.Cache, d.Clock, d.Log, d.Options),
	}
}

// Service returns the module's service, for other modules to consume through
// the interface they declare in their own ports.
func (m *Module) Service() *Service { return m.svc }
