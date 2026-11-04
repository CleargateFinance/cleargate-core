package decisionlog

import (
	"context"
	"fmt"
	"time"

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

// AgentDecisions reads one page along decision_by_agent, the index the
// migration made for it.
//
// The cursor is applied twice. The row comparison is what is exact, and the
// plain bound on evaluated_at beside it is what the index and partition
// pruning can use, so a late page does not scan the months after it.
func (r *postgresRepo) AgentDecisions(ctx context.Context, agentID id.AgentID, q Query) ([]Entry, error) {
	const stmt = `
		SELECT d.id, d.payment_id, d.outcome, d.reason_code, d.reason_text,
		       d.mandate_id, d.mandate_version, d.mandate_digest,
		       d.evaluated_at, d.latency_ms, c.stages, c.results
		FROM decision d
		CROSS JOIN LATERAL (
			SELECT array_agg(stage ORDER BY ordinal) AS stages,
			       array_agg(result ORDER BY ordinal) AS results
			FROM decision_check
			WHERE decision_id = d.id AND evaluated_at = d.evaluated_at
		) c
		WHERE d.agent_id = $1
		  AND ($2::text IS NULL OR d.outcome = $2)
		  AND ($3::timestamptz IS NULL
		       OR (d.evaluated_at <= $3 AND (d.evaluated_at, d.id) < ($3, $4::uuid)))
		ORDER BY d.evaluated_at DESC, d.id DESC
		LIMIT $5`

	var outcome, afterAt, afterID any
	if q.Outcome != "" {
		outcome = q.Outcome
	}
	if q.After != nil {
		afterAt, afterID = q.After.EvaluatedAt, string(q.After.ID)
	}

	rows, err := r.db.Query(ctx, stmt, string(agentID), outcome, afterAt, afterID, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("decisionlog: read decisions: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		var e Entry
		var decisionID, paymentID string
		var mandateID *string
		var mandateVersion *int
		var mandateDigest []byte
		var latencyMS int64
		var stages, results []string
		if err := rows.Scan(&decisionID, &paymentID, &e.Outcome, &e.ReasonCode, &e.Reason,
			&mandateID, &mandateVersion, &mandateDigest,
			&e.EvaluatedAt, &latencyMS, &stages, &results); err != nil {
			return nil, fmt.Errorf("decisionlog: scan decision: %w", err)
		}

		e.ID, e.PaymentID = id.DecisionID(decisionID), id.PaymentID(paymentID)
		e.Latency = time.Duration(latencyMS) * time.Millisecond
		if mandateID != nil && mandateVersion != nil {
			e.Mandate = &MandateRef{ID: id.MandateID(*mandateID), Version: *mandateVersion, Digest: mandateDigest}
		}
		e.Checks = make([]Check, len(stages))
		for i := range stages {
			e.Checks[i] = Check{Stage: stages[i], Result: results[i]}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("decisionlog: read decisions: %w", err)
	}
	return out, nil
}
