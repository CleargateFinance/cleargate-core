package decisionlog

import (
	"context"
	"fmt"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
)

// postgresRepo is the only place in this module that knows SQL exists.
type postgresRepo struct {
	db *database.DB
}

// NewPostgresRepository builds the Postgres-backed store.
func NewPostgresRepository(db *database.DB) Repository {
	return &postgresRepo{db: db}
}

// EnsurePartitions creates any missing monthly partitions. The naming and
// range logic is a database function next to the tables it maintains, as the
// ledger's is.
func (r *postgresRepo) EnsurePartitions(ctx context.Context, monthsAhead int) error {
	const q = `SELECT ensure_decision_partitions(1, $1)`

	if _, err := r.db.Exec(ctx, q, monthsAhead); err != nil {
		return fmt.Errorf("decisionlog: ensure partitions: %w", err)
	}
	return nil
}
