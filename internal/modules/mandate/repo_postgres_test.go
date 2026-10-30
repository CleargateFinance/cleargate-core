//go:build integration

package mandate

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// repoFixture is a real database holding one account with one agent, and the
// mandate service over it. The account module is faked at its port: who may
// issue is that module's policy and is tested there, while everything this
// module asks of the database is tested here against the real thing.
type repoFixture struct {
	db      *fixtures.TestDB
	svc     *Service
	clock   *fixedClock
	userID  id.UserID
	agentID id.AgentID
}

func newRepoFixture(t *testing.T) *repoFixture {
	t.Helper()
	ctx := context.Background()
	db := fixtures.NewTestDB(t)

	var principalID, userID, accountID, agentID string
	require.NoError(t, db.QueryRow(ctx,
		`INSERT INTO principal (kind) VALUES ('person') RETURNING id`).Scan(&principalID))
	require.NoError(t, db.QueryRow(ctx,
		`INSERT INTO app_user (principal_id, email, password_hash)
		 VALUES ($1, 'owner@example.com', 'x') RETURNING id`, principalID).Scan(&userID))
	require.NoError(t, db.QueryRow(ctx,
		`INSERT INTO account (principal_id, display_name) VALUES ($1, 'Acme') RETURNING id`,
		principalID).Scan(&accountID))
	require.NoError(t, db.QueryRow(ctx,
		`INSERT INTO agent (account_id, name) VALUES ($1, 'invoice-runner') RETURNING id`,
		accountID).Scan(&agentID))

	clock := &fixedClock{now: testNow}
	accounts := &fakeAccounts{agent: Agent{
		ID: id.AgentID(agentID), AccountID: id.AccountID(accountID), Active: true,
	}}

	return &repoFixture{
		db:      db,
		svc:     NewService(NewPostgresRepository(db.DB), accounts, clock, nil),
		clock:   clock,
		userID:  id.UserID(userID),
		agentID: id.AgentID(agentID),
	}
}

func (f *repoFixture) issue(t *testing.T, r Rules) Mandate {
	t.Helper()
	m, err := f.svc.Issue(context.Background(), Actor{UserID: f.userID}, f.agentID, r)
	require.NoError(t, err)
	return m
}

// insertRaw writes a mandate row directly, bypassing the service, the way a
// buggy migration, a support script or an attacker with database access
// would. Tests use it to show what the database refuses on its own.
func (f *repoFixture) insertRaw(ctx context.Context, version int, payload []byte, status Status) error {
	_, err := f.db.Exec(ctx,
		`INSERT INTO mandate (agent_id, version, payload, signature_scheme, signed_by, status, effective_from)
		 VALUES ($1, $2, $3, 'session_stepup', $4, $5, $6)`,
		string(f.agentID), version, payload, string(f.userID), string(status), testNow)
	return err
}

func (f *repoFixture) payload(t *testing.T, version int, r Rules) []byte {
	t.Helper()
	b, err := Canonicalise(f.agentID, version, r)
	require.NoError(t, err)
	return b
}

func TestIssue_SupersedesPrevious(t *testing.T) {
	ctx := context.Background()
	f := newRepoFixture(t)

	first := f.issue(t, validRules(t))
	assert.Equal(t, 1, first.Version)

	f.clock.now = testNow.Add(time.Hour)
	looser := validRules(t)
	looser.MaxPerTx = amount(t, "50")
	second := f.issue(t, looser)
	assert.Equal(t, 2, second.Version)

	current, err := f.svc.Current(ctx, f.agentID)
	require.NoError(t, err)
	assert.Equal(t, second.ID, current.ID)
	assert.True(t, current.Rules.MaxPerTx.Equal(amount(t, "50")))

	// The first is kept, closed at the instant the second took over.
	var status string
	var effectiveTo time.Time
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT status, effective_to FROM mandate WHERE id = $1`, string(first.ID)).
		Scan(&status, &effectiveTo))
	assert.Equal(t, string(StatusSuperseded), status)
	assert.True(t, effectiveTo.Equal(second.EffectiveFrom))
}

// TestActiveAt_ReturnsHistoricalVersion is the dispute query: a payment made
// before a change is judged by the rules in force then.
func TestActiveAt_ReturnsHistoricalVersion(t *testing.T) {
	ctx := context.Background()
	f := newRepoFixture(t)

	first := f.issue(t, validRules(t))
	f.clock.now = testNow.Add(time.Hour)
	second := f.issue(t, validRules(t))

	before, err := f.svc.At(ctx, f.agentID, testNow.Add(30*time.Minute))
	require.NoError(t, err)
	assert.Equal(t, first.ID, before.ID)

	at, err := f.svc.At(ctx, f.agentID, testNow.Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, second.ID, at.ID, "the boundary instant belongs to the newer version")

	_, err = f.svc.At(ctx, f.agentID, testNow.Add(-time.Second))
	assert.ErrorIs(t, err, ErrNoMandate, "nothing was in force before the first mandate")
}

func TestOnlyOneActiveVersion(t *testing.T) {
	ctx := context.Background()
	f := newRepoFixture(t)
	f.issue(t, validRules(t))

	err := f.insertRaw(ctx, 2, f.payload(t, 2, validRules(t)), StatusActive)
	require.Error(t, err)
	assert.True(t, isUniqueViolation(err), "a second active mandate must be refused by the index: %v", err)
}

func TestOnlyOnePendingVersion(t *testing.T) {
	ctx := context.Background()
	f := newRepoFixture(t)

	require.NoError(t, f.insertRaw(ctx, 1, f.payload(t, 1, validRules(t)), StatusPending))
	err := f.insertRaw(ctx, 2, f.payload(t, 2, validRules(t)), StatusPending)
	require.Error(t, err)
	assert.True(t, isUniqueViolation(err), "a second queued proposal must be refused: %v", err)
}

// TestPendingMandateIsNotEnforced: a proposal waiting out a timelock has no
// end date either, and must still not be mistaken for the mandate in force.
func TestPendingMandateIsNotEnforced(t *testing.T) {
	ctx := context.Background()
	f := newRepoFixture(t)

	require.NoError(t, f.insertRaw(ctx, 1, f.payload(t, 1, validRules(t)), StatusPending))

	_, err := f.svc.Current(ctx, f.agentID)
	assert.ErrorIs(t, err, ErrNoMandate)
	_, err = f.svc.At(ctx, f.agentID, testNow.Add(time.Hour))
	assert.ErrorIs(t, err, ErrNoMandate)
}

// TestUpdate_IsRejected: rules change by inserting a version, never by
// editing one, and history is never deleted.
func TestUpdate_IsRejected(t *testing.T) {
	ctx := context.Background()
	f := newRepoFixture(t)
	m := f.issue(t, validRules(t))

	looser := validRules(t)
	looser.MaxPerTx = amount(t, "99")

	attempts := map[string]struct{ sql, refusal string }{
		"payload":    {`UPDATE mandate SET payload = $2 WHERE id = $1`, "immutable"},
		"projection": {`UPDATE mandate SET max_per_tx = 99 WHERE id = $1 AND $2::bytea IS NOT NULL`, "immutable"},
		"signer":     {`UPDATE mandate SET signed_by = signed_by, effective_from = effective_from - interval '1 day' WHERE id = $1 AND $2::bytea IS NOT NULL`, "immutable"},
		"delete":     {`DELETE FROM mandate WHERE id = $1 AND $2::bytea IS NOT NULL`, "append-only"},
	}
	for name, a := range attempts {
		t.Run(name, func(t *testing.T) {
			_, err := f.db.Exec(ctx, a.sql, string(m.ID), f.payload(t, 1, looser))
			require.Error(t, err)
			assert.Contains(t, err.Error(), a.refusal)
		})
	}

	current, err := f.svc.Current(ctx, f.agentID)
	require.NoError(t, err)
	assert.Equal(t, m.Payload, current.Payload)
}

func TestStatus_OnlyMovesForward(t *testing.T) {
	ctx := context.Background()
	f := newRepoFixture(t)
	first := f.issue(t, validRules(t))
	f.issue(t, validRules(t))

	_, err := f.db.Exec(ctx,
		`UPDATE mandate SET status = 'active', effective_to = NULL WHERE id = $1`, string(first.ID))
	assert.Error(t, err, "a superseded mandate cannot be brought back into force")
}

// TestProjections_FollowPayload: the queryable columns are derived from the
// bytes, whatever the writer supplies, and the digest is of exactly those
// bytes.
func TestProjections_FollowPayload(t *testing.T) {
	ctx := context.Background()
	f := newRepoFixture(t)
	payload := f.payload(t, 1, validRules(t))

	_, err := f.db.Exec(ctx,
		`INSERT INTO mandate (agent_id, version, payload, signature_scheme, signed_by, status,
		                      effective_from, asset, max_per_tx, max_per_day, max_per_month,
		                      categories, pay_to, expires_at)
		 VALUES ($1, 1, $2, 'session_stepup', $3, 'active', $4,
		         'FAKE', 1000000, 1000000, 1000000, '{anything}', '{}', $5)`,
		string(f.agentID), payload, string(f.userID), testNow, testNow.Add(24*365*time.Hour))
	require.NoError(t, err)

	var (
		asset            string
		maxPerTx, maxDay string
		categories       []string
		digest           []byte
		expiresAt        time.Time
	)
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT asset, max_per_tx::text, max_per_day::text, categories, payload_digest, expires_at
		 FROM mandate WHERE agent_id = $1`, string(f.agentID)).
		Scan(&asset, &maxPerTx, &maxDay, &categories, &digest, &expiresAt))

	assert.Equal(t, string(testAsset), asset)
	assert.Equal(t, "20.000000000000000000", maxPerTx)
	assert.Equal(t, "100.000000000000000000", maxDay)
	assert.Equal(t, []string{"data_api", "inference"}, categories)
	sum := sha256.Sum256(payload)
	assert.Equal(t, sum[:], digest)
	assert.True(t, expiresAt.Equal(validRules(t).normalised().ExpiresAt))
}

// TestDatabaseRefusesWhatGoRefuses: the checks in Rules.Validate hold even
// for a row written around the service.
func TestDatabaseRefusesWhatGoRefuses(t *testing.T) {
	ctx := context.Background()
	f := newRepoFixture(t)

	cases := map[string]func(*Rules){
		"per-transaction above daily": func(r *Rules) { r.MaxPerTx = amount(t, "150") },
		"no categories":               func(r *Rules) { r.Categories = []string{} },
		"already expired":             func(r *Rules) { r.ExpiresAt = testNow.Add(-time.Hour) },
		"cap finer than storage":      func(r *Rules) { r.MaxPerTx = amount(t, "1.0000000000000000001") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := validRules(t)
			mutate(&r)
			assert.Error(t, f.insertRaw(ctx, 1, f.payload(t, 1, r), StatusActive))
		})
	}
}

// TestInsert_RejectsPayloadForAnotherRow: approved bytes cannot be filed
// under another agent, or as another version of the same one.
func TestInsert_RejectsPayloadForAnotherRow(t *testing.T) {
	ctx := context.Background()
	f := newRepoFixture(t)

	otherAgent, err := Canonicalise("0b8e2f4c-1d2a-4b7e-9c3f-5a6d7e8f9a0b", 1, validRules(t))
	require.NoError(t, err)
	assert.Error(t, f.insertRaw(ctx, 1, otherAgent, StatusActive))

	assert.Error(t, f.insertRaw(ctx, 2, f.payload(t, 1, validRules(t)), StatusActive))
	assert.Error(t, f.insertRaw(ctx, 1, []byte(`not json`), StatusActive))
}

// TestStepUpMandateCarriesNoAssertion: a half-recorded WebAuthn assertion
// cannot pass as either kind of approval.
func TestStepUpMandateCarriesNoAssertion(t *testing.T) {
	ctx := context.Background()
	f := newRepoFixture(t)

	_, err := f.db.Exec(ctx,
		`INSERT INTO mandate (agent_id, version, payload, signature_scheme, signature, signed_by, status, effective_from)
		 VALUES ($1, 1, $2, 'session_stepup', '\x01', $3, 'active', $4)`,
		string(f.agentID), f.payload(t, 1, validRules(t)), string(f.userID), testNow)
	assert.Error(t, err)

	_, err = f.db.Exec(ctx,
		`INSERT INTO mandate (agent_id, version, payload, signature_scheme, signature, signed_by, status, effective_from)
		 VALUES ($1, 1, $2, 'webauthn', '\x01', $3, 'active', $4)`,
		string(f.agentID), f.payload(t, 1, validRules(t)), string(f.userID), testNow)
	assert.Error(t, err)
}

// TestConcurrentIssues_NeitherSilentlyWins: racing issues for one agent leave
// one active mandate and a gap-free version history, and every loser is told.
func TestConcurrentIssues_NeitherSilentlyWins(t *testing.T) {
	ctx := context.Background()
	f := newRepoFixture(t)

	const n = 8
	rules := validRules(t)
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = f.svc.Issue(ctx, Actor{UserID: f.userID}, f.agentID, rules)
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
			continue
		}
		assert.True(t, errors.Is(err, ErrConcurrentChange), "unexpected error: %v", err)
	}
	assert.GreaterOrEqual(t, succeeded, 1)

	var active, total, maxVersion int
	require.NoError(t, f.db.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE status = 'active'), count(*), max(version)
		 FROM mandate WHERE agent_id = $1`, string(f.agentID)).
		Scan(&active, &total, &maxVersion))
	assert.Equal(t, 1, active)
	assert.Equal(t, succeeded, total)
	assert.Equal(t, total, maxVersion, "versions have no gaps")
}

// TestCurrent_RefusesStoredWebAuthnMandate is the integration form of
// TestCurrent_RefusesUnverifiableScheme: the database accepts a complete
// assertion, and the service still will not enforce what it cannot verify.
func TestCurrent_RefusesStoredWebAuthnMandate(t *testing.T) {
	ctx := context.Background()
	f := newRepoFixture(t)

	_, err := f.db.Exec(ctx,
		`INSERT INTO user_webauthn_credential (user_id, credential_id, public_key, name, backup_eligible, backup_state)
		 VALUES ($1, '\xc0ffee', '\x00', 'laptop', false, false)`, string(f.userID))
	require.NoError(t, err)

	_, err = f.db.Exec(ctx,
		`INSERT INTO mandate (agent_id, version, payload, signature_scheme, signature,
		                      authenticator_data, client_data_json, credential_id, signed_by,
		                      status, effective_from)
		 VALUES ($1, 1, $2, 'webauthn', '\x01', '\x02', '\x03', '\xc0ffee', $3, 'active', $4)`,
		string(f.agentID), f.payload(t, 1, validRules(t)), string(f.userID), testNow)
	require.NoError(t, err)

	_, err = f.svc.Current(ctx, f.agentID)
	assert.ErrorIs(t, err, ErrUnverifiable)
}
