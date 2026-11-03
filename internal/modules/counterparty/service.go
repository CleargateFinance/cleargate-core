package counterparty

import (
	"context"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Service implements this module's use cases. Depends on the interfaces
// declared in ports.go, never on repo_postgres.go or handler_http.go.
type Service struct {
	repo  Repository
	clock Clock
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// NewService builds a Service. Passing a nil clock selects the real one.
func NewService(repo Repository, clock Clock) *Service {
	if clock == nil {
		clock = systemClock{}
	}
	return &Service{repo: repo, clock: clock}
}

// Resolve returns the counterparty a payment is made to, creating it on the
// first payment anyone makes to it (§11: the directory is built from
// traffic). It returns nil when the payment names nothing to identify a payee
// by, which is not an error: such a payment can still be judged and declined.
func (s *Service) Resolve(ctx context.Context, o Observation) (*id.CounterpartyID, error) {
	identity, ok := Identify(o)
	if !ok {
		return nil, nil
	}
	cp, err := s.repo.Resolve(ctx, identity, s.clock.Now())
	if err != nil {
		return nil, err
	}
	return &cp, nil
}
