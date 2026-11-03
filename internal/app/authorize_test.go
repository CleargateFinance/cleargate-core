//go:build integration

package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/outbox"
	"github.com/CleargateFinance/cleargate-core/internal/modules/account"
	"github.com/CleargateFinance/cleargate-core/internal/modules/authz"
	"github.com/CleargateFinance/cleargate-core/internal/modules/counterparty"
	"github.com/CleargateFinance/cleargate-core/internal/modules/decisionlog"
	"github.com/CleargateFinance/cleargate-core/internal/modules/ledger"
	"github.com/CleargateFinance/cleargate-core/internal/modules/mandate"
	"github.com/CleargateFinance/cleargate-core/internal/modules/payment"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// These tests drive payment.Authorize through the same adapters the
// application wires, against one real database, because what they check is
// the transaction that spans the modules: money never moves without its
// decision, and a retry never pays twice.

var devnetRail = payment.Rail{
	Name: "x402-exact-solana", Token: "USDC", Network: "solana:EtWTRABZaYq6iMfeYKouRu166VU2xqa1",
}

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type authorizeFixture struct {
	t      *testing.T
	db     *fixtures.TestDB
	clock  *testClock
	ledger *ledger.Service
	deps   payment.Deps
	svc    *payment.Service

	accountID id.AccountID

	// agentID holds a mandate: 20 per payment, 100 a day, 200 a month, in
	// data_api only. bareAgentID, on the same account, holds none.
	agentID     id.AgentID
	bareAgentID id.AgentID
	userID      string
}

// issueMandate makes version the agent's mandate in force, superseding the
// one before it the way mandate issuance does.
func (f *authorizeFixture) issueMandate(version int, maxPerTx string) {
	ctx := context.Background()
	payload := fmt.Sprintf(`{"agent_id":%q,"asset":"USD","categories":["data_api"],`+
		`"expires_at":"2030-01-01T00:00:00Z","format":1,"max_per_day":"100",`+
		`"max_per_month":"200","max_per_tx":%q,"pay_to":[],"version":%d}`, f.agentID, maxPerTx, version)
	require.NoError(f.t, f.db.UoW.Do(ctx, func(ctx context.Context) error {
		if _, err := f.db.Exec(ctx, `
			UPDATE mandate SET status = 'superseded', effective_to = GREATEST(now(), effective_from)
			WHERE agent_id = $1 AND status = 'active'`, string(f.agentID)); err != nil {
			return err
		}
		_, err := f.db.Exec(ctx,
			`INSERT INTO mandate (agent_id, version, payload, signature_scheme, signed_by, status, effective_from)
			 VALUES ($1, $2, $3, 'session_stepup', $4, 'active', $5)`,
			string(f.agentID), version, []byte(payload), f.userID, f.clock.Now().Add(-time.Hour))
		return err
	}))
}

func newAuthorizeFixture(t *testing.T, balance string) *authorizeFixture {
	t.Helper()
	ctx := context.Background()
	db := fixtures.NewTestDB(t)
	// Inside the partitions the migration creates, which follow real time.
	clock := &testClock{now: time.Now().UTC().Truncate(time.Microsecond)}

	f := &authorizeFixture{t: t, db: db, clock: clock, ledger: ledger.New(db.DB, clock).Service()}

	var principalID, userID, accountID, agentID, bareAgentID string
	require.NoError(t, db.QueryRow(ctx,
		`INSERT INTO principal (kind) VALUES ('person') RETURNING id`).Scan(&principalID))
	require.NoError(t, db.QueryRow(ctx,
		`INSERT INTO app_user (principal_id, email, password_hash) VALUES ($1, 'owner@example.com', 'x')
		 RETURNING id`, principalID).Scan(&userID))
	require.NoError(t, db.QueryRow(ctx,
		`INSERT INTO account (principal_id, display_name) VALUES ($1, 'acme') RETURNING id`,
		principalID).Scan(&accountID))
	require.NoError(t, db.QueryRow(ctx,
		`INSERT INTO agent (account_id, name) VALUES ($1, 'buyer') RETURNING id`, accountID).Scan(&agentID))
	require.NoError(t, db.QueryRow(ctx,
		`INSERT INTO agent (account_id, name) VALUES ($1, 'idle') RETURNING id`, accountID).Scan(&bareAgentID))

	f.accountID, f.agentID, f.bareAgentID = id.AccountID(accountID), id.AgentID(agentID), id.AgentID(bareAgentID)
	f.userID = userID
	f.issueMandate(1, "20")
	f.fund(balance)

	f.deps = payment.Deps{
		DB:    db.DB,
		Clock: clock,
		// Agent and SpendingBlocked read only the repository, so the account
		// service needs none of its login machinery here.
		Accounts:       PaymentAccounts{Accounts: account.NewService(account.NewPostgresRepository(db.DB), nil, nil, nil, nil, nil, clock, nil, account.Options{})},
		Mandates:       PaymentMandates{Mandates: mandate.NewService(mandate.NewPostgresRepository(db.DB), nil, clock, nil)},
		Counterparties: PaymentCounterparties{Counterparties: counterparty.New(db.DB, clock).Service()},
		Ledger:         PaymentLedger{Ledger: f.ledger},
		Decisions:      PaymentDecisions{Decisions: decisionlog.New(db.DB).Service()},
		Outbox:         outbox.NewWriter(db.DB),
		Rail:           devnetRail,
	}
	f.rebuild()
	return f
}

// rebuild constructs the service from f.deps, after a test replaced a port.
func (f *authorizeFixture) rebuild() {
	m, err := payment.New(f.deps)
	require.NoError(f.t, err)
	f.svc = m.Service()
}

// fund credits the account from the on-chain float, the way a deposit does.
func (f *authorizeFixture) fund(amount string) {
	ctx := context.Background()
	funds, err := f.ledger.EnsureAccount(ctx, &f.accountID, ledger.TypeCustomerFunds, currency.USD)
	require.NoError(f.t, err)
	float, err := f.ledger.EnsureAccount(ctx, nil, ledger.TypeOnchainFloat, currency.USD)
	require.NoError(f.t, err)
	_, err = f.ledger.Post(ctx, ledger.Entry{
		Kind: ledger.KindFunding, Asset: currency.USD,
		Lines: ledger.Transfer(float, funds, usd(f.t, amount)),
	})
	require.NoError(f.t, err)
}

func usd(t *testing.T, s string) currency.Amount {
	t.Helper()
	a, err := currency.Parse(s, currency.USD)
	require.NoError(t, err)
	return a
}

func (f *authorizeFixture) request(key, amount string) payment.Request {
	return payment.Request{
		AccountID:      f.accountID,
		AgentID:        f.agentID,
		IdempotencyKey: key,
		Amount:         usd(f.t, amount),
		Category:       "data_api",
		PayTo:          "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU",
		Signals: authz.Signals{
			ResourceURL: "https://api.example.com/v1/quote",
			URLSource:   authz.URLSourceTask,
			Purpose:     "price lookup",
		},
	}
}

func (f *authorizeFixture) balance() string {
	b, err := f.ledger.AccountBalance(context.Background(), f.accountID, currency.USD)
	require.NoError(f.t, err)
	return b.String()
}

func (f *authorizeFixture) daySpent(agentID id.AgentID) string {
	s, err := f.ledger.SpentIn(context.Background(), agentID, ledger.DayPeriod(f.clock.Now()), currency.USD)
	require.NoError(f.t, err)
	return s.String()
}

func (f *authorizeFixture) count(query string, args ...any) int {
	var n int
	require.NoError(f.t, f.db.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}

func (f *authorizeFixture) payments() int { return f.count(`SELECT count(*) FROM payment`) }

func (f *authorizeFixture) paymentPostings() int {
	return f.count(`SELECT count(*) FROM journal_entry WHERE kind = 'payment'`)
}

// failingDecisions fails Record while fail is set, the way a full disk or a
// lost connection would, and otherwise passes through.
type failingDecisions struct {
	fail *atomic.Bool
	next payment.DecisionLog
}

func (d failingDecisions) Record(ctx context.Context, r payment.DecisionRecord) error {
	if d.fail.Load() {
		return errors.New("disk full")
	}
	return d.next.Record(ctx, r)
}

func TestAuthorize_ApprovalDebitsCountsAndRecords(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	ctx := context.Background()

	res, err := f.svc.Authorize(ctx, f.request("k1", "0.40"))
	require.NoError(t, err)

	assert.Equal(t, payment.StatusAuthorized, res.Status)
	assert.Equal(t, authz.ReasonOK, res.ReasonCode)
	assert.False(t, res.Replayed)
	assert.Equal(t, "49.6", f.balance())
	assert.Equal(t, "0.4", f.daySpent(f.agentID))

	var outcome string
	var version int
	var digest []byte
	var checks int
	require.NoError(t, f.db.QueryRow(ctx, `
		SELECT d.outcome, d.mandate_version, d.mandate_digest,
		       (SELECT count(*) FROM decision_check c WHERE c.decision_id = d.id)
		FROM decision d WHERE d.payment_id = $1`, string(res.PaymentID)).Scan(&outcome, &version, &digest, &checks))
	assert.Equal(t, "approved", outcome)
	assert.Equal(t, 1, version)
	assert.Len(t, digest, 32, "the decision names the exact bytes it was checked against")
	assert.Equal(t, 7, checks, "every stage, the skipped ones included")

	assert.Equal(t, 1, f.count(`SELECT count(*) FROM journal_entry WHERE reference_id = $1 AND agent_id = $2`,
		string(res.PaymentID), string(f.agentID)))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM event_outbox WHERE aggregate_id = $1 AND event_type = 'payment.authorized'`,
		string(res.PaymentID)))
}

func TestAuthorize_DeclineIsRecordedAndMovesNothing(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	ctx := context.Background()

	res, err := f.svc.Authorize(ctx, f.request("k1", "25"))
	require.NoError(t, err, "a decline is an answer, not an error")

	assert.Equal(t, payment.StatusDeclined, res.Status)
	assert.Equal(t, authz.ReasonOverPerTxCap, res.ReasonCode)
	assert.Equal(t, "50", f.balance())
	assert.Equal(t, "0", f.daySpent(f.agentID))
	assert.Zero(t, f.paymentPostings())
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM decision WHERE payment_id = $1 AND reason_code = 'over_per_tx_cap'`,
		string(res.PaymentID)))
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM event_outbox WHERE event_type = 'payment.declined'`))
}

// TestAuthorize_NoMandateIsARecordedDecline: an agent without authority that
// tries to pay is the attempt the console exists to show (§12.1).
func TestAuthorize_NoMandateIsARecordedDecline(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	req := f.request("k1", "1")
	req.AgentID = f.bareAgentID

	res, err := f.svc.Authorize(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, authz.ReasonNoMandate, res.ReasonCode)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM decision WHERE payment_id = $1 AND mandate_id IS NULL`,
		string(res.PaymentID)))
}

func TestAuthorize_FrozenAccountIsARecordedDecline(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	_, err := f.db.Exec(context.Background(), `UPDATE account SET frozen_at = now() WHERE id = $1`, string(f.accountID))
	require.NoError(t, err)

	res, err := f.svc.Authorize(context.Background(), f.request("k1", "1"))
	require.NoError(t, err)

	assert.Equal(t, authz.ReasonAccountBlocked, res.ReasonCode)
	assert.Equal(t, "50", f.balance())
}

func TestAuthorize_RevokedAgentIsRefused(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	_, err := f.db.Exec(context.Background(),
		`UPDATE agent SET status = 'revoked', revoked_at = now() WHERE id = $1`, string(f.agentID))
	require.NoError(t, err)

	_, err = f.svc.Authorize(context.Background(), f.request("k1", "1"))

	require.ErrorIs(t, err, payment.ErrAgentRevoked)
	assert.Zero(t, f.payments())
}

// TestAuthorize_LedgerAndDecisionAreAtomic: if the decision record cannot be
// written, the money must not move. A payment with no evidence behind it is
// the one state the product cannot survive. In a split into services this
// needs a saga; here it is a rollback.
func TestAuthorize_LedgerAndDecisionAreAtomic(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	fail := &atomic.Bool{}
	fail.Store(true)
	f.deps.Decisions = failingDecisions{fail: fail, next: f.deps.Decisions}
	f.rebuild()

	_, err := f.svc.Authorize(context.Background(), f.request("k1", "0.40"))
	require.Error(t, err)

	assert.Equal(t, "50", f.balance())
	assert.Zero(t, f.paymentPostings())
	assert.Equal(t, "0", f.daySpent(f.agentID))
	assert.Zero(t, f.payments())
	assert.Zero(t, f.count(`SELECT count(*) FROM idempotency_key`))
	assert.Zero(t, f.count(`SELECT count(*) FROM event_outbox`))
}

func TestAuthorize_ReplayReturnsOriginalResult(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	ctx := context.Background()

	first, err := f.svc.Authorize(ctx, f.request("k1", "0.40"))
	require.NoError(t, err)
	f.clock.Advance(time.Hour)
	again, err := f.svc.Authorize(ctx, f.request("k1", "0.40"))
	require.NoError(t, err)

	assert.True(t, again.Replayed)
	assert.Equal(t, first.PaymentID, again.PaymentID)
	assert.True(t, first.DecidedAt.Equal(again.DecidedAt))
	assert.Equal(t, first.ReasonCode, again.ReasonCode)
	assert.Equal(t, "49.6", f.balance(), "charged once")
	assert.Equal(t, 1, f.payments())
}

// TestAuthorize_ReplayOfADeclineDoesNotReEvaluate: the remembered answer is
// the answer, even if the account could now afford it.
func TestAuthorize_ReplayOfADeclineDoesNotReEvaluate(t *testing.T) {
	f := newAuthorizeFixture(t, "1")
	ctx := context.Background()

	first, err := f.svc.Authorize(ctx, f.request("k1", "5"))
	require.NoError(t, err)
	require.Equal(t, authz.ReasonInsufficientFunds, first.ReasonCode)

	f.fund("50")
	again, err := f.svc.Authorize(ctx, f.request("k1", "5"))
	require.NoError(t, err)

	assert.Equal(t, authz.ReasonInsufficientFunds, again.ReasonCode)
	assert.Equal(t, first.PaymentID, again.PaymentID)
}

func TestAuthorize_SameKeyDifferentBodyConflicts(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	ctx := context.Background()

	_, err := f.svc.Authorize(ctx, f.request("k1", "0.40"))
	require.NoError(t, err)
	_, err = f.svc.Authorize(ctx, f.request("k1", "0.41"))

	require.ErrorIs(t, err, payment.ErrKeyReused)
	assert.Equal(t, 1, f.payments())
}

func TestAuthorize_DifferentKeySameParamsChargesTwice(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	ctx := context.Background()

	a, err := f.svc.Authorize(ctx, f.request("k1", "0.40"))
	require.NoError(t, err)
	b, err := f.svc.Authorize(ctx, f.request("k2", "0.40"))
	require.NoError(t, err)

	assert.NotEqual(t, a.PaymentID, b.PaymentID)
	assert.Equal(t, "49.2", f.balance())
}

func TestAuthorize_ServerErrorIsNotRemembered(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	fail := &atomic.Bool{}
	fail.Store(true)
	f.deps.Decisions = failingDecisions{fail: fail, next: f.deps.Decisions}
	f.rebuild()
	ctx := context.Background()

	_, err := f.svc.Authorize(ctx, f.request("k1", "0.40"))
	require.Error(t, err)

	fail.Store(false)
	res, err := f.svc.Authorize(ctx, f.request("k1", "0.40"))
	require.NoError(t, err)

	assert.False(t, res.Replayed, "the failure left nothing to replay")
	assert.Equal(t, payment.StatusAuthorized, res.Status)
	assert.Equal(t, "49.6", f.balance())
}

// TestAuthorize_ConcurrentSameKeyChargesOnce: the retry storm of §8.4. Every
// request gets the one payment's answer, and the account is charged once.
func TestAuthorize_ConcurrentSameKeyChargesOnce(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	const n = 10

	results := make([]payment.Result, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = f.svc.Authorize(context.Background(), f.request("storm", "0.40"))
		}()
	}
	wg.Wait()

	for i := range n {
		require.NoError(t, errs[i])
		assert.Equal(t, results[0].PaymentID, results[i].PaymentID)
	}
	assert.Equal(t, 1, f.payments())
	assert.Equal(t, "49.6", f.balance())
	assert.Equal(t, "0.4", f.daySpent(f.agentID))
}

// TestAuthorize_ConcurrentPaymentsCannotExceedDailyCap: §8.5's double spend.
// Each payment alone fits under the cap and every one reads the same counter,
// but together they cannot pass it.
func TestAuthorize_ConcurrentPaymentsCannotExceedDailyCap(t *testing.T) {
	f := newAuthorizeFixture(t, "500")
	approved := f.concurrently(10, "15")

	assert.Equal(t, 6, approved, "six payments of 15 fit under a daily cap of 100")
	assert.Equal(t, "90", f.daySpent(f.agentID))
	assert.Equal(t, "410", f.balance())
	assert.Equal(t, 4, f.count(`SELECT count(*) FROM decision WHERE reason_code = 'over_daily_cap'`),
		"each loser is recorded as the decline it became")
}

func TestAuthorize_ConcurrentPaymentsCannotOverdraw(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	approved := f.concurrently(10, "10")

	assert.Equal(t, 5, approved)
	assert.Equal(t, "0", f.balance())
	assert.Equal(t, "50", f.daySpent(f.agentID))
	assert.Equal(t, 5, f.count(`SELECT count(*) FROM decision WHERE reason_code = 'insufficient_funds'`))
}

// concurrently sends n payments of amount under distinct keys at once and
// returns how many were approved, checking that every one was answered and
// that the ledger agrees with the answers.
func (f *authorizeFixture) concurrently(n int, amount string) int {
	f.t.Helper()
	results := make([]payment.Result, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = f.svc.Authorize(context.Background(), f.request(fmt.Sprintf("k%d", i), amount))
		}()
	}
	wg.Wait()

	approved := 0
	for i := range n {
		require.NoError(f.t, errs[i])
		if results[i].Status == payment.StatusAuthorized {
			approved++
		}
	}
	require.Equal(f.t, n, f.payments(), "every attempt is recorded")
	require.Equal(f.t, approved, f.paymentPostings(), "a posting for each approval and no other")
	return approved
}

// TestIdempotencyKey_ExpiresAfterRetention: after 24 hours the key is free,
// and the same request is a new payment (Task 5.2).
func TestIdempotencyKey_ExpiresAfterRetention(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	ctx := context.Background()

	first, err := f.svc.Authorize(ctx, f.request("k1", "0.40"))
	require.NoError(t, err)

	f.clock.Advance(payment.KeyRetention + time.Minute)
	second, err := f.svc.Authorize(ctx, f.request("k1", "0.40"))
	require.NoError(t, err)

	assert.False(t, second.Replayed)
	assert.NotEqual(t, first.PaymentID, second.PaymentID)
	assert.Equal(t, "49.2", f.balance())
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM idempotency_key`), "the expired row was replaced, not updated")

	f.clock.Advance(payment.KeyRetention + time.Minute)
	deleted, err := payment.NewRetention(f.db.DB, f.clock).ExpireKeys(ctx)
	require.NoError(t, err)
	assert.Equal(t, int64(1), deleted)
	assert.Zero(t, f.count(`SELECT count(*) FROM idempotency_key`))
}

// hookedLedger runs a hook once, after the last read Authorize makes before
// its transaction, so a test can land a change exactly in that gap. Debit can
// be paused, to hold a payment inside its transaction.
type hookedLedger struct {
	payment.Ledger
	once        *sync.Once
	afterReads  func()
	debitEnters chan struct{}
	debitWaits  chan struct{}
}

func (l hookedLedger) Balance(ctx context.Context, accountID id.AccountID, asset currency.Asset) (currency.Amount, error) {
	b, err := l.Ledger.Balance(ctx, accountID, asset)
	if l.afterReads != nil {
		l.once.Do(l.afterReads)
	}
	return b, err
}

func (l hookedLedger) Debit(ctx context.Context, d payment.Debit) (bool, error) {
	if l.debitEnters != nil {
		close(l.debitEnters)
		<-l.debitWaits
	}
	return l.Ledger.Debit(ctx, d)
}

func (f *authorizeFixture) afterReads(hook func()) {
	f.deps.Ledger = hookedLedger{Ledger: f.deps.Ledger, once: &sync.Once{}, afterReads: hook}
	f.rebuild()
}

// TestAuthorize_FreezeAfterTheReadsStillStopsThePayment: freezing is instant
// (Task 2.8). A payment that read the account before the freeze committed
// must not be approved after it.
func TestAuthorize_FreezeAfterTheReadsStillStopsThePayment(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	f.afterReads(func() {
		_, err := f.db.Exec(context.Background(), `UPDATE account SET frozen_at = now() WHERE id = $1`, string(f.accountID))
		require.NoError(t, err)
	})

	res, err := f.svc.Authorize(context.Background(), f.request("k1", "0.40"))
	require.NoError(t, err)

	assert.Equal(t, authz.ReasonAccountBlocked, res.ReasonCode, "decided again, and recorded as the decline it became")
	assert.Equal(t, "50", f.balance())
	assert.Zero(t, f.paymentPostings())
}

func TestAuthorize_RevocationAfterTheReadsStillStopsThePayment(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	f.afterReads(func() {
		_, err := f.db.Exec(context.Background(),
			`UPDATE agent SET status = 'revoked', revoked_at = now() WHERE id = $1`, string(f.agentID))
		require.NoError(t, err)
	})

	_, err := f.svc.Authorize(context.Background(), f.request("k1", "0.40"))

	require.ErrorIs(t, err, payment.ErrAgentRevoked)
	assert.Equal(t, "50", f.balance())
	assert.Zero(t, f.payments())
}

// TestAuthorize_NewMandateAfterTheReadsIsTheOneApplied: once a new version
// is issued, no payment is approved against the old one.
func TestAuthorize_NewMandateAfterTheReadsIsTheOneApplied(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	f.afterReads(func() { f.issueMandate(2, "0.10") })

	res, err := f.svc.Authorize(context.Background(), f.request("k1", "0.40"))
	require.NoError(t, err)

	assert.Equal(t, authz.ReasonOverPerTxCap, res.ReasonCode)
	assert.Equal(t, 1, f.count(`SELECT count(*) FROM decision WHERE payment_id = $1 AND mandate_version = 2`,
		string(res.PaymentID)))
	assert.Equal(t, "50", f.balance())
}

// TestAuthorize_FreezeWaitsForAPaymentAlreadyCommitting: the other order. A
// payment holding the account inside its transaction makes a freeze wait,
// so when the freeze returns, nothing approved can still be on its way.
func TestAuthorize_FreezeWaitsForAPaymentAlreadyCommitting(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	enters, waits := make(chan struct{}), make(chan struct{})
	f.deps.Ledger = hookedLedger{Ledger: f.deps.Ledger, debitEnters: enters, debitWaits: waits}
	f.rebuild()
	// Registered after the fixture, so it runs before the database closes:
	// a failed assertion must still let the paused payment finish, or closing
	// the pool waits for its connection forever.
	release := sync.OnceFunc(func() { close(waits) })
	t.Cleanup(release)

	done := make(chan error, 1)
	go func() {
		_, err := f.svc.Authorize(context.Background(), f.request("k1", "0.40"))
		done <- err
	}()
	<-enters

	err := f.db.UoW.Do(context.Background(), func(ctx context.Context) error {
		if _, err := f.db.Exec(ctx, `SET LOCAL lock_timeout = '200ms'`); err != nil {
			return err
		}
		_, err := f.db.Exec(ctx, `UPDATE account SET frozen_at = now() WHERE id = $1`, string(f.accountID))
		return err
	})
	require.Error(t, err, "the freeze must wait for the payment holding the account")
	assert.Contains(t, err.Error(), "lock timeout")

	release()
	require.NoError(t, <-done)
	assert.Equal(t, "49.6", f.balance())
}

// TestAuthorize_EveryPaymentNamesItsCounterparty: the directory is built from
// traffic (§11), declines included, and two spellings of one endpoint are
// one payee.
func TestAuthorize_EveryPaymentNamesItsCounterparty(t *testing.T) {
	f := newAuthorizeFixture(t, "50")
	ctx := context.Background()

	approved, err := f.svc.Authorize(ctx, f.request("k1", "0.40"))
	require.NoError(t, err)
	declined := f.request("k2", "25")
	declined.Signals.ResourceURL = "HTTPS://API.EXAMPLE.COM/v1/quote/?session=abc"
	declinedRes, err := f.svc.Authorize(ctx, declined)
	require.NoError(t, err)
	require.Equal(t, payment.StatusDeclined, declinedRes.Status)

	noURL := f.request("k3", "0.40")
	noURL.Signals.ResourceURL = ""
	noURLRes, err := f.svc.Authorize(ctx, noURL)
	require.NoError(t, err)

	key := func(p payment.Result) string {
		var k string
		require.NoError(t, f.db.QueryRow(ctx, `
			SELECT c.canonical_key FROM payment p JOIN counterparty c ON c.id = p.counterparty_id
			WHERE p.id = $1`, string(p.PaymentID)).Scan(&k))
		return k
	}
	assert.Equal(t, "endpoint:api.example.com/v1/quote", key(approved))
	assert.Equal(t, key(approved), key(declinedRes))
	assert.Equal(t, "address:"+devnetRail.Network+":7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU", key(noURLRes))
}
