//go:build integration

package payment

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/modules/authz"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// These tests write rows directly, the way a buggy script, a later code path
// or an operator would, to show what the schema refuses on its own. The
// service that writes them properly arrives with Task 5.3.

const devnet = "solana:EtWTRABZaYq6iMfeYKouRu166VU2xqa1"

type schemaFixture struct {
	db *fixtures.TestDB

	accountID, agentID           string
	otherAccountID, otherAgentID string

	// Version 1 of agentID's mandate, and otherAgentID's.
	mandateID, otherMandateID string
	mandateDigest             []byte

	// Inside the partitions the migration bootstraps.
	at time.Time
}

func newSchemaFixture(t *testing.T) *schemaFixture {
	t.Helper()
	ctx := context.Background()
	f := &schemaFixture{db: fixtures.NewTestDB(t), at: time.Now().UTC().Truncate(time.Microsecond)}

	var principalID, userID string
	require.NoError(t, f.db.QueryRow(ctx,
		`INSERT INTO principal (kind) VALUES ('person') RETURNING id`).Scan(&principalID))
	require.NoError(t, f.db.QueryRow(ctx,
		`INSERT INTO app_user (principal_id, email, password_hash)
		 VALUES ($1, 'owner@example.com', 'x') RETURNING id`, principalID).Scan(&userID))

	newAgent := func(name string) (accountID, agentID string) {
		require.NoError(t, f.db.QueryRow(ctx,
			`INSERT INTO account (principal_id, display_name) VALUES ($1, $2) RETURNING id`,
			principalID, name).Scan(&accountID))
		require.NoError(t, f.db.QueryRow(ctx,
			`INSERT INTO agent (account_id, name) VALUES ($1, $2) RETURNING id`,
			accountID, name+"-agent").Scan(&agentID))
		return accountID, agentID
	}
	f.accountID, f.agentID = newAgent("acme")
	f.otherAccountID, f.otherAgentID = newAgent("globex")

	newMandate := func(agentID string) (mandateID string, digest []byte) {
		payload := fmt.Sprintf(`{"agent_id":%q,"asset":"USD","categories":["data_api"],`+
			`"expires_at":"2030-01-01T00:00:00Z","format":1,"max_per_day":"100",`+
			`"max_per_month":"200","max_per_tx":"20","pay_to":[],"version":1}`, agentID)
		require.NoError(t, f.db.QueryRow(ctx,
			`INSERT INTO mandate (agent_id, version, payload, signature_scheme, signed_by, status, effective_from)
			 VALUES ($1, 1, $2, 'session_stepup', $3, 'active', $4)
			 RETURNING id, payload_digest`,
			agentID, []byte(payload), userID, f.at.Add(-time.Hour)).Scan(&mandateID, &digest))
		return mandateID, digest
	}
	f.mandateID, f.mandateDigest = newMandate(f.agentID)
	f.otherMandateID, _ = newMandate(f.otherAgentID)

	return f
}

type paymentRow struct {
	accountID, agentID string
	amount             string
	status             string
}

type decisionRow struct {
	outcome, reason string

	// The mandate columns, or all nil for none.
	mandateID      any
	mandateVersion any
	mandateDigest  any

	features string
}

func (f *schemaFixture) approved() (paymentRow, decisionRow) {
	return paymentRow{f.accountID, f.agentID, "0.40", "authorized"},
		decisionRow{"approved", "ok", f.mandateID, 1, f.mandateDigest, `{"v":1,"mandate":{}}`}
}

func (f *schemaFixture) declinedNoMandate() (paymentRow, decisionRow) {
	return paymentRow{f.accountID, f.agentID, "0.40", "declined"},
		decisionRow{"declined", "no_mandate", nil, nil, nil, `{"v":1,"mandate":null}`}
}

// record writes a payment and its decision in one transaction, the way
// Task 5.3 will, and returns the payment's id with whatever error the
// transaction or its commit raised.
func (f *schemaFixture) record(p paymentRow, d decisionRow) (string, error) {
	var paymentID string
	err := f.db.UoW.Do(context.Background(), func(ctx context.Context) error {
		var err error
		paymentID, err = f.insertPayment(ctx, p)
		if err != nil {
			return err
		}
		_, err = f.insertDecision(ctx, paymentID, f.at, d)
		return err
	})
	return paymentID, err
}

func (f *schemaFixture) insertPayment(ctx context.Context, p paymentRow) (string, error) {
	var resolvedAt any
	if p.status != "authorized" {
		resolvedAt = f.at
	}
	var paymentID string
	err := f.db.QueryRow(ctx,
		`INSERT INTO payment (account_id, agent_id, amount, asset, category, pay_to,
		                      rail, token, network, status, decided_at, resolved_at)
		 VALUES ($1, $2, $3, 'USD', 'data_api', 'payee-address',
		         'x402-exact-solana', 'USDC', $4, $5, $6, $7)
		 RETURNING id`,
		p.accountID, p.agentID, p.amount, devnet, p.status, f.at, resolvedAt).Scan(&paymentID)
	return paymentID, err
}

func (f *schemaFixture) insertDecision(ctx context.Context, paymentID string, at time.Time, d decisionRow) (string, error) {
	var decisionID string
	err := f.db.QueryRow(ctx,
		`INSERT INTO decision (payment_id, account_id, agent_id, outcome, reason_code, reason_text,
		                       mandate_id, mandate_version, mandate_digest, features,
		                       evaluated_at, latency_ms)
		 SELECT id, account_id, agent_id, $2, $3, 'reason', $4, $5, $6, $7, $8, 3
		   FROM payment WHERE id = $1
		 RETURNING id`,
		paymentID, d.outcome, d.reason, d.mandateID, d.mandateVersion, d.mandateDigest,
		d.features, at).Scan(&decisionID)
	return decisionID, err
}

func TestPayment_RecordsApprovalsAndDeclines(t *testing.T) {
	f := newSchemaFixture(t)

	_, err := f.record(f.approved())
	require.NoError(t, err)

	_, err = f.record(f.declinedNoMandate())
	require.NoError(t, err, "a decline is a full payment with a decision behind it")

	p, d := f.declinedNoMandate()
	p.amount, d.reason = "-5", "invalid_amount"
	_, err = f.record(p, d)
	require.NoError(t, err, "the invalid_amount decline records the amount as requested")
}

func TestPayment_WithoutDecisionIsRefusedAtCommit(t *testing.T) {
	f := newSchemaFixture(t)
	p, _ := f.approved()

	err := f.db.UoW.Do(context.Background(), func(ctx context.Context) error {
		_, err := f.insertPayment(ctx, p)
		return err
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no decision")
}

func TestDecision_OutcomeMustMatchPayment(t *testing.T) {
	f := newSchemaFixture(t)

	p, _ := f.approved()
	_, d := f.declinedNoMandate()
	_, err := f.record(p, d)
	require.Error(t, err, "an authorized payment with a declining decision")

	p, _ = f.declinedNoMandate()
	_, d = f.approved()
	_, err = f.record(p, d)
	require.Error(t, err, "a declined payment with an approving decision")
}

func TestDecision_OnlyOnePerPayment(t *testing.T) {
	ctx := context.Background()
	f := newSchemaFixture(t)
	paymentID, err := f.record(f.approved())
	require.NoError(t, err)
	_, d := f.approved()

	_, err = f.insertDecision(ctx, paymentID, f.at, d)
	assert.Error(t, err, "a second decision at the payment's decision time")

	_, err = f.insertDecision(ctx, paymentID, f.at.Add(time.Second), d)
	assert.Error(t, err, "a second decision at any other time")
}

// TestDecision_NamesTheExactMandateEvaluated: a decision's mandate columns
// must describe a mandate that exists, for this agent, with exactly this
// version and these bytes. Otherwise the evidence could point at rules nobody
// evaluated.
func TestDecision_NamesTheExactMandateEvaluated(t *testing.T) {
	f := newSchemaFixture(t)

	cases := map[string]func(*decisionRow){
		"another agent's mandate": func(d *decisionRow) { d.mandateID = f.otherMandateID },
		"a version it never had":  func(d *decisionRow) { d.mandateVersion = 2 },
		"different bytes":         func(d *decisionRow) { d.mandateDigest = make([]byte, 32) },
		"id without the digest":   func(d *decisionRow) { d.mandateDigest = nil },
		"id without the version":  func(d *decisionRow) { d.mandateVersion = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p, d := f.approved()
			mutate(&d)
			_, err := f.record(p, d)
			assert.Error(t, err)
		})
	}
}

func TestDecision_MandateIsAbsentOnlyWhenItCouldBe(t *testing.T) {
	f := newSchemaFixture(t)

	// Checked before the mandate, so an agent without one can fail them.
	for _, reason := range []string{"no_mandate", "invalid_amount", "account_blocked"} {
		t.Run(reason+" without a mandate is recorded", func(t *testing.T) {
			p, d := f.declinedNoMandate()
			d.reason = reason
			_, err := f.record(p, d)
			assert.NoError(t, err)
		})
	}

	t.Run("a later decline without a mandate is refused", func(t *testing.T) {
		p, d := f.declinedNoMandate()
		d.reason = "over_per_tx_cap"
		_, err := f.record(p, d)
		assert.Error(t, err)
	})

	t.Run("no_mandate naming a mandate is refused", func(t *testing.T) {
		p, _ := f.declinedNoMandate()
		_, d := f.approved()
		d.outcome, d.reason = "declined", "no_mandate"
		_, err := f.record(p, d)
		assert.Error(t, err)
	})
}

func TestDecision_FeaturesAgreeWithColumns(t *testing.T) {
	f := newSchemaFixture(t)

	cases := map[string]string{
		"no version":                    `{"mandate":{}}`,
		"version not a number":          `{"v":"1","mandate":{}}`,
		"no mandate key":                `{"v":1}`,
		"null mandate beside a mandate": `{"v":1,"mandate":null}`,
	}
	for name, features := range cases {
		t.Run(name, func(t *testing.T) {
			p, d := f.approved()
			d.features = features
			_, err := f.record(p, d)
			assert.Error(t, err)
		})
	}

	t.Run("a mandate in the features beside none in the columns", func(t *testing.T) {
		p, d := f.declinedNoMandate()
		d.features = `{"v":1,"mandate":{}}`
		_, err := f.record(p, d)
		assert.Error(t, err)
	})
}

// TestDecision_ReasonCodesMatchTheEngine: the table accepts exactly the codes
// authz can produce. A code added to the engine without a migration, or the
// reverse, fails here instead of in production.
func TestDecision_ReasonCodesMatchTheEngine(t *testing.T) {
	f := newSchemaFixture(t)

	for _, code := range authz.ReasonCodes() {
		t.Run(string(code), func(t *testing.T) {
			p, d := f.approved()
			if code != authz.ReasonOK {
				p.status, d.outcome, d.reason = "declined", "declined", string(code)
			}
			if code == authz.ReasonNoMandate {
				d.mandateID, d.mandateVersion, d.mandateDigest = nil, nil, nil
				d.features = `{"v":1,"mandate":null}`
			}
			_, err := f.record(p, d)
			assert.NoError(t, err)
		})
	}

	p, d := f.declinedNoMandate()
	d.reason = "made_up"
	_, err := f.record(p, d)
	assert.Error(t, err)
}

func TestDecision_IsAppendOnly(t *testing.T) {
	ctx := context.Background()
	f := newSchemaFixture(t)
	paymentID, err := f.record(f.approved())
	require.NoError(t, err)

	var decisionID string
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT id FROM decision WHERE payment_id = $1`, paymentID).Scan(&decisionID))
	_, err = f.db.Exec(ctx,
		`INSERT INTO decision_check (decision_id, evaluated_at, ordinal, stage, result)
		 VALUES ($1, $2, 1, 'request', 'pass')`, decisionID, f.at)
	require.NoError(t, err)

	for _, stmt := range []string{
		`UPDATE decision SET reason_text = 'reworded' WHERE id = $1`,
		`DELETE FROM decision WHERE id = $1`,
		`UPDATE decision_check SET result = 'fail' WHERE decision_id = $1`,
		`DELETE FROM decision_check WHERE decision_id = $1`,
	} {
		_, err := f.db.Exec(ctx, stmt, decisionID)
		require.Error(t, err, stmt)
		assert.Contains(t, err.Error(), "append-only")
	}
}

func TestDecisionCheck_EachStageOnce(t *testing.T) {
	ctx := context.Background()
	f := newSchemaFixture(t)
	paymentID, err := f.record(f.approved())
	require.NoError(t, err)
	var decisionID string
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT id FROM decision WHERE payment_id = $1`, paymentID).Scan(&decisionID))

	insert := func(ordinal int, stage, result string) error {
		_, err := f.db.Exec(ctx,
			`INSERT INTO decision_check (decision_id, evaluated_at, ordinal, stage, result)
			 VALUES ($1, $2, $3, $4, $5)`, decisionID, f.at, ordinal, stage, result)
		return err
	}
	require.NoError(t, insert(1, "request", "pass"))
	assert.Error(t, insert(2, "request", "pass"), "the same stage twice")
	assert.Error(t, insert(1, "account", "pass"), "the same position twice")
	assert.Error(t, insert(3, "vibes", "pass"), "an unknown stage")
	assert.Error(t, insert(4, "mandate", "maybe"), "an unknown result")
}

func TestPayment_StatusMovesForwardOnce(t *testing.T) {
	ctx := context.Background()
	f := newSchemaFixture(t)

	authorized, err := f.record(f.approved())
	require.NoError(t, err)
	declined, err := f.record(f.declinedNoMandate())
	require.NoError(t, err)

	settle := `UPDATE payment SET status = $2, resolved_at = $3 WHERE id = $1`
	later := f.at.Add(time.Minute)

	_, err = f.db.Exec(ctx, settle, declined, "settled", later)
	assert.Error(t, err, "a decline is final")

	_, err = f.db.Exec(ctx, `UPDATE payment SET status = 'settled' WHERE id = $1`, authorized)
	assert.Error(t, err, "a final status needs its resolution time")

	_, err = f.db.Exec(ctx, settle, authorized, "settled", later)
	require.NoError(t, err)

	_, err = f.db.Exec(ctx, settle, authorized, "expired", later)
	assert.Error(t, err, "settled is final")

	_, err = f.db.Exec(ctx, `UPDATE payment SET amount = 1000 WHERE id = $1`, declined)
	assert.Error(t, err, "the request is immutable")

	_, err = f.db.Exec(ctx, `DELETE FROM payment WHERE id = $1`, declined)
	assert.Error(t, err)
}

func TestPayment_RefusesInconsistentRows(t *testing.T) {
	f := newSchemaFixture(t)

	cases := map[string]func(*paymentRow){
		"another account's agent":             func(p *paymentRow) { p.agentID = f.otherAgentID },
		"starting settled":                    func(p *paymentRow) { p.status = "settled" },
		"a zero amount that was approved":     func(p *paymentRow) { p.amount = "0" },
		"a negative amount that was approved": func(p *paymentRow) { p.amount = "-1" },
		"an unknown status":                   func(p *paymentRow) { p.status = "pending" },
		"an amount that is not a number":      func(p *paymentRow) { p.amount = "abc" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p, d := f.approved()
			mutate(&p)
			_, err := f.record(p, d)
			assert.Error(t, err)
		})
	}
}

func TestIdempotencyKey_RemembersOutcomeImmutably(t *testing.T) {
	ctx := context.Background()
	f := newSchemaFixture(t)
	paymentID, err := f.record(f.approved())
	require.NoError(t, err)

	insert := `INSERT INTO idempotency_key (account_id, key, request_hash, payment_id, response)
	           VALUES ($1, $2, $3, $4, '{}')`
	hash := make([]byte, 32)

	_, err = f.db.Exec(ctx, insert, f.accountID, "k1", hash, paymentID)
	require.NoError(t, err)

	_, err = f.db.Exec(ctx, insert, f.accountID, "k1", hash, paymentID)
	assert.Error(t, err, "the same key twice in one account")

	_, err = f.db.Exec(ctx, insert, f.otherAccountID, "k2", hash, paymentID)
	assert.Error(t, err, "a key in one account naming another account's payment")

	_, err = f.db.Exec(ctx, insert, f.accountID, "k3", []byte("short"), paymentID)
	assert.Error(t, err, "a hash that is not sha256")

	_, err = f.db.Exec(ctx, `UPDATE idempotency_key SET response = '{"x":1}' WHERE key = 'k1'`)
	assert.Error(t, err, "a remembered outcome never changes")

	_, err = f.db.Exec(ctx, `DELETE FROM idempotency_key WHERE key = 'k1'`)
	assert.NoError(t, err, "expired keys are deleted")
}

func TestEventOutbox_MarkedPublishedOnceThenDeletable(t *testing.T) {
	ctx := context.Background()
	f := newSchemaFixture(t)

	var eventID int64
	require.NoError(t, f.db.QueryRow(ctx,
		`INSERT INTO event_outbox (aggregate_type, aggregate_id, event_type, payload)
		 VALUES ('payment', gen_random_uuid(), 'payment.authorized', '{}') RETURNING id`).Scan(&eventID))

	_, err := f.db.Exec(ctx, `DELETE FROM event_outbox WHERE id = $1`, eventID)
	assert.Error(t, err, "an unpublished event is not deletable")

	_, err = f.db.Exec(ctx, `UPDATE event_outbox SET payload = '{"x":1}' WHERE id = $1`, eventID)
	assert.Error(t, err, "an event's content never changes")

	_, err = f.db.Exec(ctx, `UPDATE event_outbox SET published_at = now() WHERE id = $1`, eventID)
	require.NoError(t, err)

	_, err = f.db.Exec(ctx, `UPDATE event_outbox SET published_at = now() WHERE id = $1`, eventID)
	assert.Error(t, err, "published once")

	_, err = f.db.Exec(ctx, `DELETE FROM event_outbox WHERE id = $1`, eventID)
	assert.NoError(t, err)
}
