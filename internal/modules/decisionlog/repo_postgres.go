package decisionlog

import (
	"context"
	"fmt"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
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

// Insert writes the decision and its checks in one statement, one round trip
// on the 100ms path. Ordinals count from 1 in the order the checks ran.
func (r *postgresRepo) Insert(ctx context.Context, rec Record) (id.DecisionID, error) {
	const q = `
		WITH d AS (
			INSERT INTO decision (payment_id, account_id, agent_id, outcome, reason_code, reason_text,
			                      mandate_id, mandate_version, mandate_digest,
			                      features, evaluated_at, latency_ms)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			RETURNING id, evaluated_at
		), c AS (
			INSERT INTO decision_check (decision_id, evaluated_at, ordinal, stage, result)
			SELECT d.id, d.evaluated_at, c.ordinal, c.stage, c.result
			FROM d, unnest($13::smallint[], $14::text[], $15::text[]) AS c (ordinal, stage, result)
		)
		SELECT id FROM d`

	var mandateID, mandateVersion, mandateDigest any
	if m := rec.Mandate; m != nil {
		mandateID, mandateVersion, mandateDigest = string(m.ID), m.Version, m.Digest
	}

	ordinals := make([]int16, len(rec.Checks))
	stages := make([]string, len(rec.Checks))
	results := make([]string, len(rec.Checks))
	for i, c := range rec.Checks {
		ordinals[i], stages[i], results[i] = int16(i+1), c.Stage, c.Result // #nosec G115 -- a handful of pipeline stages
	}

	var out string
	err := r.db.QueryRow(ctx, q,
		string(rec.PaymentID), string(rec.AccountID), string(rec.AgentID),
		rec.Outcome, rec.ReasonCode, rec.Reason,
		mandateID, mandateVersion, mandateDigest,
		rec.Features, rec.EvaluatedAt, rec.Latency.Milliseconds(),
		ordinals, stages, results,
	).Scan(&out)
	if err != nil {
		return "", fmt.Errorf("decisionlog: insert decision: %w", err)
	}
	return id.DecisionID(out), nil
}
