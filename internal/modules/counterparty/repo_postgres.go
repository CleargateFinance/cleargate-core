package counterparty

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// postgresRepo is the only file in this module that knows SQL exists.
type postgresRepo struct {
	db *database.DB
}

// NewPostgresRepository builds the Postgres-backed store.
func NewPostgresRepository(db *database.DB) Repository {
	return &postgresRepo{db: db}
}

// Resolve is an upsert rather than a select-then-insert, so two first
// payments to a new endpoint cannot create it twice; the loser of that race
// reads back the winner's row. The database derives canonical_key from the
// columns, and Identity.Key computes the same string to find it.
func (r *postgresRepo) Resolve(ctx context.Context, i Identity, at time.Time) (id.CounterpartyID, error) {
	const insert = `
		INSERT INTO counterparty (kind, domain, host, path, network, address, first_seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (canonical_key) DO NOTHING
		RETURNING id`

	var out string
	err := r.db.QueryRow(ctx, insert, string(i.Kind),
		nullable(i.Domain), nullable(i.Host), nullable(i.Path),
		nullable(i.Network), nullable(i.Address), at,
	).Scan(&out)
	if err == nil {
		return id.CounterpartyID(out), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("counterparty: create: %w", err)
	}

	if err := r.db.QueryRow(ctx,
		`SELECT id FROM counterparty WHERE canonical_key = $1`, i.Key(),
	).Scan(&out); err != nil {
		return "", fmt.Errorf("counterparty: load existing: %w", err)
	}
	return id.CounterpartyID(out), nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
