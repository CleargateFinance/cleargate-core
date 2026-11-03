package decisionlog

import (
	"context"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Service implements this module's use cases. It depends on the interfaces in
// ports.go, never on the storage implementation directly.
type Service struct {
	repo Repository
}

// NewService builds a Service.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// Record stores one decision. Call it inside the unit of work that writes the
// payment: the database refuses to commit a payment without its decision, and
// a decision cannot be written without its payment, so the two land together.
//
// The hash chain of Task 5.4 is not built yet; prev_hash, hash and signature
// stay null until it is.
func (s *Service) Record(ctx context.Context, r Record) (id.DecisionID, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	return s.repo.Insert(ctx, r)
}

// EnsurePartitions creates any monthly partitions that do not exist yet.
func (s *Service) EnsurePartitions(ctx context.Context, monthsAhead int) error {
	return s.repo.EnsurePartitions(ctx, monthsAhead)
}
