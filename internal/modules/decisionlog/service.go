package decisionlog

import "context"

// Service implements this module's use cases. It depends on the interfaces in
// ports.go, never on the storage implementation directly.
//
// Recording decisions arrives with Task 5.3. Until then the module only keeps
// its tables ready to receive them.
type Service struct {
	repo Repository
}

// NewService builds a Service.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// EnsurePartitions creates any monthly partitions that do not exist yet.
func (s *Service) EnsurePartitions(ctx context.Context, monthsAhead int) error {
	return s.repo.EnsurePartitions(ctx, monthsAhead)
}
