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
	svc := NewService(NewPostgresRepository(db.DB))

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
	svc := NewService(NewPostgresRepository(db.DB))
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

// logFixture is an account with two agents. agentID holds a mandate.
type logFixture struct {
	t   *testing.T
	db  *fixtures.TestDB
	svc *Service
	log *Log

	accountID, agentID, otherAgentID string
	mandateID                        string
	mandateDigest                    []byte

	// Inside the partitions the migration bootstraps.
	at time.Time
}

// anyPayments answers every id with an empty payment. What a decision was
// about is the payment module's test; this one is about the decisions.
type anyPayments struct{}

func (anyPayments) AgentPayments(_ context.Context, _ id.AgentID, ids []id.PaymentID) (map[id.PaymentID]Payment, error) {
	out := make(map[id.PaymentID]Payment, len(ids))
	for _, i := range ids {
		out[i] = Payment{}
	}
	return out, nil
}

func newLogFixture(t *testing.T) *logFixture {
	t.Helper()
	ctx := context.Background()
	db := fixtures.NewTestDB(t)
	repo := NewPostgresRepository(db.DB)
	f := &logFixture{
		t: t, db: db, svc: NewService(repo), log: NewLog(repo, fakeAccounts{}, anyPayments{}),
		at: time.Now().UTC().Truncate(time.Microsecond),
	}

	var principalID, userID string
	require.NoError(t, db.QueryRow(ctx,
		`INSERT INTO principal (kind) VALUES ('person') RETURNING id`).Scan(&principalID))
	require.NoError(t, db.QueryRow(ctx,
		`INSERT INTO app_user (principal_id, email, password_hash) VALUES ($1, 'owner@example.com', 'x')
		 RETURNING id`, principalID).Scan(&userID))
	require.NoError(t, db.QueryRow(ctx,
		`INSERT INTO account (principal_id, display_name) VALUES ($1, 'acme') RETURNING id`,
		principalID).Scan(&f.accountID))
	for _, agent := range []*string{&f.agentID, &f.otherAgentID} {
		require.NoError(t, db.QueryRow(ctx,
			`INSERT INTO agent (account_id, name) VALUES ($1, 'buyer') RETURNING id`, f.accountID).Scan(agent))
	}

	payload := fmt.Sprintf(`{"agent_id":%q,"asset":"USD","categories":["data_api"],`+
		`"expires_at":"2030-01-01T00:00:00Z","format":1,"max_per_day":"100",`+
		`"max_per_month":"200","max_per_tx":"20","pay_to":[],"version":1}`, f.agentID)
	require.NoError(t, db.QueryRow(ctx,
		`INSERT INTO mandate (agent_id, version, payload, signature_scheme, signed_by, status, effective_from)
		 VALUES ($1, 1, $2, 'session_stepup', $3, 'active', $4)
		 RETURNING id, payload_digest`,
		f.agentID, []byte(payload), userID, f.at.Add(-time.Hour)).Scan(&f.mandateID, &f.mandateDigest))
	return f
}

// decide records one payment and its decision for agentID at an instant: an
// approval against the mandate, or a no_mandate decline.
func (f *logFixture) decide(agentID string, outcome string, at time.Time) id.DecisionID {
	f.t.Helper()
	status, rec := "declined", Record{
		Outcome: OutcomeDeclined, ReasonCode: "no_mandate", Reason: "the agent has no mandate in force",
		Features: []byte(`{"v":1,"mandate":null}`),
		Checks:   []Check{{"request", "pass"}, {"account", "pass"}, {"mandate", "fail"}},
	}
	if outcome == OutcomeApproved {
		status, rec = "authorized", Record{
			Outcome: OutcomeApproved, ReasonCode: "ok", Reason: "within the mandate",
			Mandate:  &MandateRef{ID: id.MandateID(f.mandateID), Version: 1, Digest: f.mandateDigest},
			Features: []byte(`{"v":1,"mandate":{}}`),
			Checks:   []Check{{"request", "pass"}, {"account", "pass"}, {"mandate", "pass"}, {"funds", "pass"}},
		}
	}

	var decisionID id.DecisionID
	require.NoError(f.t, f.db.UoW.Do(context.Background(), func(ctx context.Context) error {
		var resolvedAt any
		if status == "declined" {
			resolvedAt = at
		}
		var paymentID string
		if err := f.db.QueryRow(ctx, `
			INSERT INTO payment (account_id, agent_id, amount, asset, rail, token, network,
			                     status, decided_at, resolved_at)
			VALUES ($1, $2, 0.40, 'USD', 'x402-exact-solana', 'USDC',
			        'solana:EtWTRABZaYq6iMfeYKouRu166VU2xqa1', $3, $4, $5)
			RETURNING id`, f.accountID, agentID, status, at, resolvedAt).Scan(&paymentID); err != nil {
			return err
		}
		rec.PaymentID, rec.AccountID, rec.AgentID = id.PaymentID(paymentID), id.AccountID(f.accountID), id.AgentID(agentID)
		rec.EvaluatedAt, rec.Latency = at, 4*time.Millisecond
		var err error
		decisionID, err = f.svc.Record(ctx, rec)
		return err
	}))
	return decisionID
}

func (f *logFixture) page(q Query) Page {
	f.t.Helper()
	page, err := f.log.AgentDecisions(context.Background(), Actor{UserID: "u"}, id.AgentID(f.agentID), q)
	require.NoError(f.t, err)
	return page
}

func idsOf(entries []Entry) []id.DecisionID {
	out := make([]id.DecisionID, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

// TestAgentDecisions_NewestFirstWithWhatWasChecked: an approval names the
// mandate version it was checked against, a no_mandate decline names none,
// and both carry their stages in the order they ran.
func TestAgentDecisions_NewestFirstWithWhatWasChecked(t *testing.T) {
	f := newLogFixture(t)
	approved := f.decide(f.agentID, OutcomeApproved, f.at.Add(-time.Second))
	declined := f.decide(f.agentID, OutcomeDeclined, f.at)

	page := f.page(Query{})

	require.Equal(t, []id.DecisionID{declined, approved}, idsOf(page.Entries))
	assert.Nil(t, page.Next)

	d := page.Entries[0]
	assert.Equal(t, "no_mandate", d.ReasonCode)
	assert.Equal(t, "the agent has no mandate in force", d.Reason)
	assert.Nil(t, d.Mandate)
	assert.Equal(t, []Check{{"request", "pass"}, {"account", "pass"}, {"mandate", "fail"}}, d.Checks)
	assert.True(t, f.at.Equal(d.EvaluatedAt))
	assert.Equal(t, 4*time.Millisecond, d.Latency)

	a := page.Entries[1]
	assert.Equal(t, OutcomeApproved, a.Outcome)
	require.NotNil(t, a.Mandate)
	assert.Equal(t, MandateRef{ID: id.MandateID(f.mandateID), Version: 1, Digest: f.mandateDigest}, *a.Mandate)
	assert.Len(t, a.Checks, 4)
}

// TestAgentDecisions_PagesThroughSharedInstantsWithoutGapsOrRepeats: one
// agent's payments can be decided in the same microsecond. A page boundary
// that fell between two of them must neither skip nor repeat either.
func TestAgentDecisions_PagesThroughSharedInstantsWithoutGapsOrRepeats(t *testing.T) {
	f := newLogFixture(t)
	for i := range 7 {
		// Three instants, so most page boundaries land inside a tie.
		f.decide(f.agentID, OutcomeDeclined, f.at.Add(-time.Duration(i/3)*time.Second))
	}
	all := idsOf(f.page(Query{}).Entries)
	require.Len(t, all, 7)

	var walked []id.DecisionID
	q := Query{Limit: 2}
	for range 10 {
		page := f.page(q)
		walked = append(walked, idsOf(page.Entries)...)
		if page.Next == nil {
			break
		}
		// Through the wire form, as a client would send it back.
		next, err := ParseCursor(page.Next.String())
		require.NoError(t, err)
		q.After = &next
	}

	assert.Equal(t, all, walked)
}

// TestAgentDecisions_FiltersByOutcomeAndKeepsToTheAgent: the console's
// "blocked" view is the declines alone, and an agent's log never shows a
// decision another agent's payment received.
func TestAgentDecisions_FiltersByOutcomeAndKeepsToTheAgent(t *testing.T) {
	f := newLogFixture(t)
	f.decide(f.agentID, OutcomeApproved, f.at.Add(-2*time.Second))
	declined := f.decide(f.agentID, OutcomeDeclined, f.at.Add(-time.Second))
	f.decide(f.otherAgentID, OutcomeDeclined, f.at)

	assert.Len(t, f.page(Query{}).Entries, 2, "the other agent's decision is not here")
	assert.Equal(t, []id.DecisionID{declined}, idsOf(f.page(Query{Outcome: OutcomeDeclined}).Entries))
}
