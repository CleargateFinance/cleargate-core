//go:build integration

package decisionlog

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// TestEnsurePartitions_CoversMonthsAheadAndIsIdempotent: the worker runs this
// daily, so a second run must change nothing, and a write months from now
// must find its partition waiting.
func TestEnsurePartitions_CoversMonthsAheadAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := fixtures.NewTestDB(t)
	svc := New(db.DB).Service()

	require.NoError(t, svc.EnsurePartitions(ctx, 14))
	require.NoError(t, svc.EnsurePartitions(ctx, 14))

	month := time.Now().UTC().AddDate(0, 14, 0)
	for _, parent := range []string{"decision", "decision_check"} {
		var exists bool
		name := fmt.Sprintf("%s_%04dm%02d", parent, month.Year(), int(month.Month()))
		require.NoError(t, db.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1)`, name).Scan(&exists))
		assert.True(t, exists, name)
	}
}

// TestRecord_StoresDecisionWithItsChecksInOrder: the decision commits with the
// payment it judged, and its checks keep the order they ran in.
func TestRecord_StoresDecisionWithItsChecksInOrder(t *testing.T) {
	ctx := context.Background()
	db := fixtures.NewTestDB(t)
	svc := New(db.DB).Service()
	at := time.Now().UTC().Truncate(time.Microsecond)

	var accountID, agentID string
	require.NoError(t, db.QueryRow(ctx, `
		WITH p AS (INSERT INTO principal (kind) VALUES ('person') RETURNING id),
		     a AS (INSERT INTO account (principal_id, display_name) SELECT id, 'acme' FROM p RETURNING id)
		INSERT INTO agent (account_id, name) SELECT id, 'buyer' FROM a
		RETURNING account_id, id`).Scan(&accountID, &agentID))

	var paymentID string
	var decisionID id.DecisionID
	err := db.UoW.Do(ctx, func(ctx context.Context) error {
		if err := db.QueryRow(ctx, `
			INSERT INTO payment (account_id, agent_id, amount, asset, rail, token, network,
			                     status, decided_at, resolved_at)
			VALUES ($1, $2, 0.40, 'USD', 'x402-exact-solana', 'USDC',
			        'solana:EtWTRABZaYq6iMfeYKouRu166VU2xqa1', 'declined', $3, $3)
			RETURNING id`, accountID, agentID, at).Scan(&paymentID); err != nil {
			return err
		}
		var err error
		decisionID, err = svc.Record(ctx, Record{
			PaymentID:   id.PaymentID(paymentID),
			AccountID:   id.AccountID(accountID),
			AgentID:     id.AgentID(agentID),
			Outcome:     "declined",
			ReasonCode:  "no_mandate",
			Reason:      "the agent has no mandate in force",
			Features:    []byte(`{"v":1,"mandate":null}`),
			Checks:      []Check{{"request", "pass"}, {"account", "pass"}, {"mandate", "fail"}},
			EvaluatedAt: at,
			Latency:     3 * time.Millisecond,
		})
		return err
	})
	require.NoError(t, err)

	rows, err := db.Query(ctx, `
		SELECT c.ordinal, c.stage, c.result, d.latency_ms
		FROM decision d JOIN decision_check c USING (evaluated_at)
		WHERE d.id = $1 AND c.decision_id = d.id
		ORDER BY c.ordinal`, string(decisionID))
	require.NoError(t, err)
	defer rows.Close()

	var got []string
	for rows.Next() {
		var ordinal, latency int
		var stage, result string
		require.NoError(t, rows.Scan(&ordinal, &stage, &result, &latency))
		got = append(got, fmt.Sprintf("%d:%s:%s", ordinal, stage, result))
		assert.Equal(t, 3, latency)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"1:request:pass", "2:account:pass", "3:mandate:fail"}, got)
}

func TestRecord_RefusesARecordWithoutChecks(t *testing.T) {
	svc := NewService(nil)

	_, err := svc.Record(context.Background(), Record{
		PaymentID: "p", AccountID: "a", AgentID: "g", Outcome: "approved", ReasonCode: "ok",
		Features: []byte(`{}`), EvaluatedAt: time.Now(),
	})

	assert.Error(t, err)
}
