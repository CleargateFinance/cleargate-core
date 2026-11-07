//go:build integration

package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/config"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/solana"
	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/solana/solanatest"
	"github.com/CleargateFinance/cleargate-core/internal/modules/ledger"
	"github.com/CleargateFinance/cleargate-core/internal/modules/payment"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// These tests pay through the real API, then run the worker's own sweep against
// what the fake node reports, because what they check is the money: nothing is
// released that could still land, and nothing settles twice.

// sweeper is the worker's settlement job, at a time the test controls.
func (f *payFixture) sweeper(clock *testClock) *payment.Settler {
	f.t.Helper()
	chain, err := solana.New(solana.Options{RPCURL: f.node.URL()})
	require.NoError(f.t, err)
	settler, err := newSettler(f.db.DB, chain, f.cfg.Solana, clock, slog.New(&alertLog{}))
	require.NoError(f.t, err)
	return settler
}

func nowClock() *testClock { return &testClock{now: time.Now().UTC()} }

func (f *payFixture) sweep() {
	f.t.Helper()
	require.NoError(f.t, f.sweeper(nowClock()).Sweep(context.Background()))
}

// land puts a paid transaction on-chain, as the merchant's facilitator would.
func (f *payFixture) land(paid authorizeJSON, failed bool) {
	f.t.Helper()
	f.landWithMemo(paid, paid.PaymentID, failed)
}

// landWithMemo lands a transaction whose memo the merchant chose.
func (f *payFixture) landWithMemo(paid authorizeJSON, memo string, failed bool) {
	f.t.Helper()
	raw, err := base64.StdEncoding.DecodeString(*paid.PaymentSignature)
	require.NoError(f.t, err)
	var payload struct {
		Payload struct {
			Transaction string `json:"transaction"`
		} `json:"payload"`
	}
	require.NoError(f.t, json.Unmarshal(raw, &payload))
	f.node.Land(f.wallet, payload.Payload.Transaction, memo, failed, time.Now())
}

func (f *payFixture) status(paid authorizeJSON) string {
	f.t.Helper()
	var status string
	require.NoError(f.t, f.db.QueryRow(context.Background(),
		`SELECT status FROM payment WHERE id = $1`, paid.PaymentID).Scan(&status))
	return status
}

func (f *payFixture) daySpent() string {
	f.t.Helper()
	s, err := ledger.New(f.db.DB, nil).Service().SpentIn(context.Background(), f.agentID, ledger.DayPeriod(time.Now()), currency.USD)
	require.NoError(f.t, err)
	return s.String()
}

// platform is a platform bucket's balance.
func (f *payFixture) platform(typ ledger.AccountType) string {
	f.t.Helper()
	svc := ledger.New(f.db.DB, nil).Service()
	bucket, err := svc.EnsureAccount(context.Background(), nil, typ, currency.USD)
	require.NoError(f.t, err)
	b, err := svc.Balance(context.Background(), bucket, currency.USD)
	require.NoError(f.t, err)
	return b.String()
}

// bookIsClean checks the two invariants the worker checks: the book sums to
// zero, and the counters agree with the postings after every reversal.
func (f *payFixture) bookIsClean() {
	f.t.Helper()
	svc := ledger.New(f.db.DB, nil).Service()
	checks, err := svc.CheckBook(context.Background())
	require.NoError(f.t, err)
	for _, c := range checks {
		assert.True(f.t, c.Balanced(), "the book sums to zero in %s", c.Asset)
	}
	for _, p := range ledger.PeriodsFor(time.Now().UTC()) {
		drifts, err := svc.CheckSpendCounters(context.Background(), p)
		require.NoError(f.t, err)
		assert.Empty(f.t, drifts, "the counters agree with the postings")
	}
}

func TestConfirm_SettlesLandedTransaction(t *testing.T) {
	f := newPayFixture(t, "50")
	paid := f.pays("key-1", f.body("1000000", "data_api"))
	require.Equal(t, "1", f.platform(ledger.TypePayeePayable))

	f.land(paid, false)
	f.sweep()

	assert.Equal(t, "settled", f.status(paid))
	assert.Equal(t, "49", f.balance(), "the money stayed spent")
	assert.Equal(t, "1", f.daySpent())
	assert.Equal(t, "0", f.platform(ledger.TypePayeePayable), "nothing is owed any more")
	assert.Equal(t, "-49", f.platform(ledger.TypeOnchainFloat), "the float records what is held for customers")
	assert.Equal(t, 1, f.rows(`SELECT count(*) FROM journal_entry WHERE kind = 'payout'`))
	assert.Equal(t, 1, f.rows(`SELECT count(*) FROM event_outbox WHERE event_type = 'payment.settled'`))
	assert.Equal(t, 1, f.rows(`SELECT count(*) FROM payment WHERE resolved_at IS NOT NULL AND status = 'settled'`))
	f.bookIsClean()
}

func TestConfirm_SettlesOnce(t *testing.T) {
	f := newPayFixture(t, "50")
	paid := f.pays("key-1", f.body("1000000", "data_api"))
	f.land(paid, false)

	f.sweep()
	f.sweep()

	assert.Equal(t, 1, f.rows(`SELECT count(*) FROM journal_entry WHERE kind = 'payout'`))
	f.bookIsClean()
}

// TestConfirm_AFailedTransactionGivesEverythingBack: it landed and did nothing,
// so the money never left and the agent keeps its budget.
func TestConfirm_AFailedTransactionGivesEverythingBack(t *testing.T) {
	f := newPayFixture(t, "50")
	paid := f.pays("key-1", f.body("1000000", "data_api"))

	f.land(paid, true)
	f.sweep()

	assert.Equal(t, "failed", f.status(paid))
	assert.Equal(t, "50", f.balance())
	assert.Equal(t, "0", f.daySpent())
	assert.Equal(t, "0", f.platform(ledger.TypePayeePayable))
	assert.Equal(t, 1, f.rows(`SELECT count(*) FROM journal_entry WHERE kind = 'reversal' AND reverses_entry_id IS NOT NULL`))
	f.bookIsClean()
}

func TestExpire_ReleasesFundsAndBudget(t *testing.T) {
	f := newPayFixture(t, "50")
	paid := f.pays("key-1", f.body("1000000", "data_api"))

	f.node.SetHeight(1150 + 151) // Past the last valid height and the margin.
	f.sweep()

	assert.Equal(t, "expired", f.status(paid))
	assert.Equal(t, "50", f.balance())
	assert.Equal(t, "0", f.daySpent())
	assert.Equal(t, "0", f.platform(ledger.TypePayeePayable))
	assert.Equal(t, 1, f.rows(`SELECT count(*) FROM event_outbox WHERE event_type = 'payment.expired'`))
	f.bookIsClean()

	// The budget really is back: a new payment can spend it.
	again := f.pays("key-2", f.body("20000000", "data_api"))
	assert.Equal(t, "authorized", again.Status)
}

func TestExpire_NeverBeforeBlockhashExpires(t *testing.T) {
	f := newPayFixture(t, "50")
	paid := f.pays("key-1", f.body("1000000", "data_api"))

	for _, height := range []uint64{1000, 1150, 1150 + 150} {
		f.node.SetHeight(height)
		f.sweep()
		assert.Equal(t, "authorized", f.status(paid), "height %d", height)
	}
	assert.Equal(t, "49", f.balance())
}

// TestExpire_ALateLandingStillSettles: seen after its height has passed, a landed
// transaction is settled, never released.
func TestExpire_ALateLandingStillSettles(t *testing.T) {
	f := newPayFixture(t, "50")
	paid := f.pays("key-1", f.body("1000000", "data_api"))

	f.land(paid, false)
	f.node.SetHeight(1_000_000)
	f.sweep()

	assert.Equal(t, "settled", f.status(paid))
	assert.Equal(t, "49", f.balance())
}

// TestExpire_ARetryAfterExpiryHandsOutNothing: the key is remembered for a day,
// but the transaction is dead, and a retry must say so.
func TestExpire_ARetryAfterExpiryHandsOutNothing(t *testing.T) {
	f := newPayFixture(t, "50")
	f.pays("key-1", f.body("1000000", "data_api"))
	f.node.SetHeight(1_000_000)
	f.sweep()

	retry := f.pays("key-1", f.body("1000000", "data_api"))

	assert.True(t, retry.Replayed)
	assert.Equal(t, "expired", retry.Status)
	assert.Nil(t, retry.PaymentSignature)
	assert.Equal(t, 1, f.cdp.Signs(), "and it does not sign a new one")
	assert.Equal(t, "50", f.balance())
}

// TestSweep_ANodeFailureReleasesNothing: not being able to read the chain must
// never look like a transfer that did not land, however late it is.
func TestSweep_ANodeFailureReleasesNothing(t *testing.T) {
	f := newPayFixture(t, "50")
	paid := f.pays("key-1", f.body("1000000", "data_api"))
	f.node.SetHeight(1_000_000)

	for _, method := range []string{"getBlockHeight", "getSignaturesForAddress"} {
		t.Run(method, func(t *testing.T) {
			f.node.Fail(method, true)
			defer f.node.Fail(method, false)

			assert.Error(t, f.sweeper(nowClock()).Sweep(context.Background()))
			assert.Equal(t, "authorized", f.status(paid))
			assert.Equal(t, "49", f.balance())
		})
	}
}

// TestSweep_AnUnsignedPaymentIsReleasedAfterItsLimit: signing failed and the agent
// never retried. No transaction exists, so nothing can land, and without this the
// funds would be stuck forever, since there is no block height to pass.
func TestSweep_AnUnsignedPaymentIsReleasedAfterItsLimit(t *testing.T) {
	f := newPayFixture(t, "50")
	f.cdp.FailSigning(true)
	rec := f.authorize("key-1", f.body("1000000", "data_api"))
	require.GreaterOrEqual(t, rec.Code, http.StatusInternalServerError)
	require.Equal(t, "49", f.balance())

	early := nowClock()
	early.Advance(payment.UnsignedExpiry - time.Minute)
	require.NoError(t, f.sweeper(early).Sweep(context.Background()))
	assert.Equal(t, 1, f.rows(`SELECT count(*) FROM payment WHERE status = 'authorized'`), "a retry may still come")

	late := nowClock()
	late.Advance(payment.UnsignedExpiry + time.Minute)
	require.NoError(t, f.sweeper(late).Sweep(context.Background()))
	assert.Equal(t, 1, f.rows(`SELECT count(*) FROM payment WHERE status = 'expired'`))
	assert.Equal(t, "50", f.balance())
	assert.Equal(t, "0", f.daySpent())
	f.bookIsClean()

	// The custodian is back and the agent finally retries: the payment is gone,
	// and no transaction leaves for money the account has already been given back.
	f.cdp.FailSigning(false)
	retry := f.pays("key-1", f.body("1000000", "data_api"))
	assert.Equal(t, "expired", retry.Status)
	assert.Nil(t, retry.PaymentSignature)
	assert.Zero(t, f.cdp.Signs())
}

// TestSweep_ARetryInTimeKeepsTheUnsignedPaymentAlive: once signed, it waits on the
// chain, not on the unsigned limit.
func TestSweep_ARetryInTimeKeepsTheUnsignedPaymentAlive(t *testing.T) {
	f := newPayFixture(t, "50")
	f.cdp.FailSigning(true)
	f.authorize("key-1", f.body("1000000", "data_api"))
	f.cdp.FailSigning(false)
	paid := f.pays("key-1", f.body("1000000", "data_api"))
	require.NotNil(t, paid.PaymentSignature)

	late := nowClock()
	late.Advance(payment.UnsignedExpiry + time.Minute)
	require.NoError(t, f.sweeper(late).Sweep(context.Background()))

	assert.Equal(t, "authorized", f.status(paid))
	assert.Equal(t, "49", f.balance())
}

// TestPayment_AReleasedPaymentCannotGainATransaction: the database's half of the
// race between a late signing and the sweep.
func TestPayment_AReleasedPaymentCannotGainATransaction(t *testing.T) {
	f := newPayFixture(t, "50")
	f.cdp.FailSigning(true)
	f.authorize("key-1", f.body("1000000", "data_api"))
	late := nowClock()
	late.Advance(payment.UnsignedExpiry + time.Minute)
	require.NoError(t, f.sweeper(late).Sweep(context.Background()))

	_, err := f.db.Exec(context.Background(), `
		INSERT INTO payment_transfer (payment_id, memo, wallet_address, signed_transaction, message_digest, payment_signature, last_valid_block_height)
		SELECT id, 'memo', $1, 'tx', decode(repeat('01', 32), 'hex'), 'sig', 1 FROM payment`, f.wallet)

	assert.ErrorContains(t, err, "cannot gain a transaction")
}

// TestMVP_DefinitionOfDone walks the plan's definition of done through the real
// API and the worker's real jobs: fund an account, connect an agent, set its
// mandate, pay, be declined, retry, settle, and balance the book. Only the chain
// and the custodian are fakes, and both speak their real wire formats.
func TestMVP_DefinitionOfDone(t *testing.T) {
	f := newAPIFixture(t)
	ctx := context.Background()
	_, err := f.db.Exec(ctx, `
		INSERT INTO principal_identity (principal_id) SELECT principal_id FROM account WHERE id = $1`, string(f.accountID))
	require.NoError(t, err)
	owner := f.session("owner@example.com", "owner", true)

	// 1. Create an account, fund it, connect an agent, sign a mandate.
	rec := f.do(http.MethodGet, "/v1/accounts/"+string(f.accountID)+"/deposit-address", "", credentials{sessionToken: owner})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	wallet := decode[depositAddressJSON](t, rec.Body.Bytes()).Address

	f.node.Transfer(false, solanaHoldings(wallet, 0, 50_000_000)...)
	chain, err := solana.New(solana.Options{RPCURL: f.node.URL()})
	require.NoError(t, err)
	deposits, err := newDepositWatcher(f.db.DB, chain, f.cfg.Solana, nil, slog.New(&alertLog{}))
	require.NoError(t, err)
	require.NoError(t, deposits.Poll(ctx))

	rec = f.do(http.MethodGet, "/v1/accounts/"+string(f.accountID)+"/summary", "", credentials{sessionToken: owner})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"amount":"50"`, "the deposit shows in the console")

	rec = f.do(http.MethodPut, "/v1/agents/"+string(f.agentID)+"/mandate", rulesJSON,
		credentials{sessionToken: owner, consoleHeader: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	p := &payFixture{apiFixture: f, wallet: wallet, feePayer: randomFeePayer(t)}

	// 2. A $0.40 payment: approved, debited, counted, recorded.
	approved := p.pays("pay-1", p.body("400000", "data_api"))
	assert.Equal(t, "authorized", approved.Status)
	require.NotNil(t, approved.PaymentSignature)
	assert.Equal(t, "49.6", p.balance())
	assert.Equal(t, "0.4", p.daySpent())

	// 3. $85 against a $20 cap: declined, nothing moves, and it shows.
	declined := p.pays("pay-2", p.body("85000000", "data_api"))
	assert.Equal(t, "declined", declined.Status)
	assert.Equal(t, "over_per_tx_cap", declined.ReasonCode)
	assert.Equal(t, "49.6", p.balance())

	rec = f.do(http.MethodGet, "/v1/agents/"+string(f.agentID)+"/decisions", "", credentials{sessionToken: owner})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "over_per_tx_cap", "with its reason")
	assert.Contains(t, rec.Body.String(), `"version":1`, "and the mandate version")

	// 4. A retry with the same key returns the original, with no second payment.
	retry := p.pays("pay-1", p.body("400000", "data_api"))
	assert.True(t, retry.Replayed)
	assert.Equal(t, *approved.PaymentSignature, *retry.PaymentSignature)
	assert.Equal(t, "49.6", p.balance())
	assert.Equal(t, 1, f.cdp.Signs())

	// 5. The approved payment confirms on-chain.
	p.land(approved, false)
	settler, err := newSettler(f.db.DB, chain, f.cfg.Solana, nil, slog.New(&alertLog{}))
	require.NoError(t, err)
	require.NoError(t, settler.Sweep(ctx))
	assert.Equal(t, "settled", p.status(approved))

	// A second payment never lands, and gives back its funds and budget.
	lost := p.pays("pay-3", p.body("1000000", "data_api"))
	f.node.SetHeight(1_000_000)
	require.NoError(t, settler.Sweep(ctx))
	assert.Equal(t, "expired", p.status(lost))
	assert.Equal(t, "49.6", p.balance())
	assert.Equal(t, "0.4", p.daySpent())

	// 6. The reconciliation job confirms the ledger sums to zero.
	p.bookIsClean()
}

func solanaHoldings(wallet string, before, after uint64) []solanatest.Holdings {
	return []solanatest.Holdings{{Owner: wallet, Mint: solanatest.DevnetUSDC, Before: before, After: after}}
}

func randomFeePayer(t *testing.T) string {
	t.Helper()
	return solanatest.RandomAddress(t)
}

// TestBuildWorker_RunsTheChainJobs builds the worker the way cmd/worker does and
// runs its new jobs. It is given no CDP credentials, because it must never need
// them: nothing the worker does can move money.
func TestBuildWorker_RunsTheChainJobs(t *testing.T) {
	db := fixtures.NewTestDB(t)
	node := solanatest.New(t)

	w, cleanup, err := BuildWorker(context.Background(), &config.Config{
		Database: config.Database{DSN: db.DSN, MaxConns: 2, MinConns: 1, ConnectTimeout: 10 * time.Second},
		Solana:   config.Solana{RPCURL: node.URL(), Network: devnetRail.Network, USDCMint: solanatest.DevnetUSDC, USDCDecimals: 6},
	}, slog.New(&alertLog{}))
	require.NoError(t, err)
	t.Cleanup(cleanup)

	ran := map[string]bool{}
	for _, j := range w.jobs {
		if j.name == "deposit_detection" || j.name == "payment_settlement" {
			assert.NoError(t, j.run(context.Background()), j.name)
			ran[j.name] = true
		}
	}
	assert.Len(t, ran, 2)
}
