//go:build integration

package app

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/modules/ledger"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
	"github.com/CleargateFinance/cleargate-core/test/fixtures"
)

// These tests run the worker's reconciliation jobs against a real database
// and read what they log, since the log line is what reaches a person.

// alertLog keeps every record logged at error, with its attributes.
type alertLog struct {
	mu      sync.Mutex
	records []map[string]string
}

func (l *alertLog) Enabled(_ context.Context, level slog.Level) bool { return level >= slog.LevelError }
func (l *alertLog) WithAttrs([]slog.Attr) slog.Handler               { return l }
func (l *alertLog) WithGroup(string) slog.Handler                    { return l }

func (l *alertLog) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]string{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, attrs)
	return nil
}

func (l *alertLog) alerts() []map[string]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]map[string]string(nil), l.records...)
}

type ledgerJobFixture struct {
	db     *fixtures.TestDB
	svc    *ledger.Service
	log    *alertLog
	logger *slog.Logger
	payer  id.LedgerAccountID
	payee  id.LedgerAccountID
	agent  id.AgentID
}

func newLedgerJobFixture(t *testing.T) *ledgerJobFixture {
	t.Helper()
	ctx := context.Background()
	db := fixtures.NewTestDB(t)
	log := &alertLog{}
	f := &ledgerJobFixture{db: db, svc: ledger.New(db.DB, nil).Service(), log: log, logger: slog.New(log)}

	var accountID, agentID string
	require.NoError(t, db.QueryRow(ctx, `
		WITH p AS (INSERT INTO principal (kind) VALUES ('person') RETURNING id),
		     a AS (INSERT INTO account (principal_id, display_name) SELECT id, 'acme' FROM p RETURNING id)
		INSERT INTO agent (account_id, name) SELECT id, 'buyer' FROM a
		RETURNING account_id, id`).Scan(&accountID, &agentID))
	account := id.AccountID(accountID)
	f.agent = id.AgentID(agentID)

	var err error
	f.payer, err = f.svc.EnsureAccount(ctx, &account, ledger.TypeCustomerFunds, currency.USD)
	require.NoError(t, err)
	f.payee, err = f.svc.EnsureAccount(ctx, nil, ledger.TypePayeePayable, currency.USD)
	require.NoError(t, err)
	float, err := f.svc.EnsureAccount(ctx, nil, ledger.TypeOnchainFloat, currency.USD)
	require.NoError(t, err)
	_, err = f.svc.Post(ctx, ledger.Entry{
		Kind: ledger.KindFunding, Asset: currency.USD,
		Lines: ledger.Transfer(float, f.payer, f.usd(t, "100")),
	})
	require.NoError(t, err)
	return f
}

func (f *ledgerJobFixture) usd(t *testing.T, s string) currency.Amount {
	t.Helper()
	a, err := currency.Parse(s, currency.USD)
	require.NoError(t, err)
	return a
}

// pay posts a payment at at, and records spend on the agent's counters.
func (f *ledgerJobFixture) pay(t *testing.T, at time.Time, posted, counted string) {
	t.Helper()
	ctx := context.Background()
	_, err := f.svc.Post(ctx, ledger.Entry{
		Kind: ledger.KindPayment, Asset: currency.USD,
		Lines:   ledger.Transfer(f.payer, f.payee, f.usd(t, posted)),
		AgentID: &f.agent, OccurredAt: at,
	})
	require.NoError(t, err)
	_, err = f.svc.RecordSpend(ctx, f.agent, f.usd(t, counted), at)
	require.NoError(t, err)
}

// TestLedgerBookCheck_ReportsAnInjectedImbalance is the test Task 1.4 names:
// an imbalance written past the database's own check reaches the error log
// with the alert that pages for it.
func TestLedgerBookCheck_ReportsAnInjectedImbalance(t *testing.T) {
	ctx := context.Background()
	f := newLedgerJobFixture(t)
	f.pay(t, time.Now().UTC(), "0.40", "0.40")

	require.NoError(t, checkBook(ctx, f.logger, f.svc))
	assert.Empty(t, f.log.alerts(), "a balanced book raises nothing")

	// session_replication_role = replica skips triggers, the balance check
	// among them, for this one transaction, as only a defect or a hand-run
	// statement could.
	tx, err := f.db.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	_, err = tx.Exec(ctx, `SET LOCAL session_replication_role = replica`)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `
		WITH je AS (
			INSERT INTO journal_entry (kind, description, created_at)
			VALUES ('funding', 'injected by a test', now()) RETURNING id, created_at
		)
		INSERT INTO posting (journal_entry_id, ledger_account_id, amount, asset, created_at)
		SELECT id, $1, 0.01, 'USD', created_at FROM je`, string(f.payer))
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))

	require.NoError(t, checkBook(ctx, f.logger, f.svc), "finding an imbalance is the job working, not failing")
	alerts := f.log.alerts()
	require.Len(t, alerts, 1)
	assert.Equal(t, "ledger_imbalance", alerts[0][alertKey])
	assert.Equal(t, "USD", alerts[0]["asset"])
	assert.Equal(t, "0.01", alerts[0]["total"])
}

// TestSpendCounterCheck_ReportsDriftInTheWindowThatClosed: drift in
// yesterday's window is found by today's run, not only drift in today's.
func TestSpendCounterCheck_ReportsDriftInTheWindowThatClosed(t *testing.T) {
	ctx := context.Background()
	f := newLedgerJobFixture(t)
	now := time.Now().UTC()
	yesterday := now.Add(-counterCheckInterval)

	f.pay(t, now, "0.40", "0.40")
	f.pay(t, yesterday, "0.40", "5.00") // a counter that disagrees with its postings

	require.NoError(t, checkSpendCounters(ctx, f.logger, f.svc, now))

	var days []string
	for _, a := range f.log.alerts() {
		assert.Equal(t, "spend_counter_drift", a[alertKey])
		assert.Equal(t, string(f.agent), a["agent_id"])
		if a["period_type"] == string(ledger.PeriodDay) {
			days = append(days, a["period_start"])
			assert.Equal(t, "5", a["counter"])
			assert.Equal(t, "0.4", a["recomputed"])
		}
	}
	assert.Equal(t, []string{ledger.DayPeriod(yesterday).Start.Format(time.DateOnly)}, days,
		"yesterday's day window is reported, and today's, which agrees, is not")
}
