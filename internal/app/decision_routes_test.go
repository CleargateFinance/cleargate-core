//go:build integration

package app

import (
	"context"
	"net/http"
	"net/url"
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
)

// These tests drive Task 6.2's decision log: payments go through the real
// authorize path, and the console reads back what was decided about each.

type decisionLogJSON struct {
	Decisions []struct {
		ID         string `json:"id"`
		Outcome    string `json:"outcome"`
		ReasonCode string `json:"reason_code"`
		Reason     string `json:"reason"`
		Mandate    *struct {
			ID      string `json:"id"`
			Version int    `json:"version"`
			Digest  string `json:"digest"`
		} `json:"mandate"`
		Checks []struct {
			Stage  string `json:"stage"`
			Result string `json:"result"`
		} `json:"checks"`
		Payment struct {
			ID          string  `json:"id"`
			Amount      string  `json:"amount"`
			Asset       string  `json:"asset"`
			Category    *string `json:"category"`
			PayTo       *string `json:"pay_to"`
			ResourceURL *string `json:"resource_url"`
			Status      string  `json:"status"`
		} `json:"payment"`
		EvaluatedAt time.Time `json:"evaluated_at"`
	} `json:"decisions"`
	NextCursor *string `json:"next_cursor"`
}

const payee = "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU"

// pay authorizes one payment by the fixture's agent through the same
// adapters the application wires.
func (f *apiFixture) pay(svc *payment.Service, key, amount string) payment.Result {
	f.t.Helper()
	a, err := currency.Parse(amount, currency.USD)
	require.NoError(f.t, err)
	res, err := svc.Authorize(context.Background(), payment.Request{
		AccountID: f.accountID, AgentID: f.agentID, IdempotencyKey: key,
		Amount: a, Category: "data_api", PayTo: payee,
		Signals: authz.Signals{ResourceURL: "https://api.example.com/v1/quote", URLSource: authz.URLSourceTask},
	})
	require.NoError(f.t, err)
	return res
}

// paymentService builds the payment path against the fixture's database and
// funds the account with balance, so approvals are possible. An empty balance
// funds nothing; every payment is then declined.
func (f *apiFixture) paymentService(balance string) *payment.Service {
	f.t.Helper()
	ctx := context.Background()
	ledgerSvc := ledger.New(f.db.DB, nil).Service()

	if balance != "" {
		funds, err := ledgerSvc.EnsureAccount(ctx, &f.accountID, ledger.TypeCustomerFunds, currency.USD)
		require.NoError(f.t, err)
		float, err := ledgerSvc.EnsureAccount(ctx, nil, ledger.TypeOnchainFloat, currency.USD)
		require.NoError(f.t, err)
		amount, err := currency.Parse(balance, currency.USD)
		require.NoError(f.t, err)
		_, err = ledgerSvc.Post(ctx, ledger.Entry{
			Kind: ledger.KindFunding, Asset: currency.USD, Lines: ledger.Transfer(float, funds, amount),
		})
		require.NoError(f.t, err)
	}

	m, err := payment.New(payment.Deps{
		DB: f.db.DB,
		Accounts: PaymentAccounts{Accounts: account.NewService(account.NewPostgresRepository(f.db.DB),
			nil, nil, nil, nil, nil, nil, nil, account.Options{})},
		Mandates:       PaymentMandates{Mandates: mandate.NewService(mandate.NewPostgresRepository(f.db.DB), nil, nil, nil)},
		Counterparties: PaymentCounterparties{Counterparties: counterparty.New(f.db.DB, nil).Service()},
		Ledger:         PaymentLedger{Ledger: ledgerSvc},
		Decisions:      PaymentDecisions{Decisions: decisionlog.NewService(decisionlog.NewPostgresRepository(f.db.DB))},
		Outbox:         outbox.NewWriter(f.db.DB),
		Rail:           devnetRail,
	})
	require.NoError(f.t, err)
	return m.Service()
}

func (f *apiFixture) decisionLog(session, query string) decisionLogJSON {
	f.t.Helper()
	rec := f.do(http.MethodGet, "/v1/agents/"+string(f.agentID)+"/decisions"+query, "",
		credentials{sessionToken: session})
	require.Equal(f.t, http.StatusOK, rec.Code, rec.Body.String())
	return decode[decisionLogJSON](f.t, rec.Body.Bytes())
}

// TestDecisionLogRoute_ShowsDeclinesBesideApprovals: an agent with no mandate
// is declined, then a mandate is set, and one payment fits it and one does
// not. A viewer reads all three back, newest first, with what each was about.
func TestDecisionLogRoute_ShowsDeclinesBesideApprovals(t *testing.T) {
	f := newAPIFixture(t)
	svc := f.paymentService("100")
	owner := f.session("owner@example.com", "owner", true)
	viewer := f.session("viewer@example.com", "viewer", false)

	noMandate := f.pay(svc, "k1", "5")
	rec := f.do(http.MethodPut, "/v1/agents/"+string(f.agentID)+"/mandate", rulesJSON,
		credentials{sessionToken: owner, consoleHeader: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	approved := f.pay(svc, "k2", "5")
	overCap := f.pay(svc, "k3", "25")
	require.Equal(t, authz.Approved, approved.Outcome)

	log := f.decisionLog(viewer, "")
	require.Len(t, log.Decisions, 3)
	assert.Nil(t, log.NextCursor)
	got := []string{log.Decisions[0].Payment.ID, log.Decisions[1].Payment.ID, log.Decisions[2].Payment.ID}
	assert.Equal(t, []string{string(overCap.PaymentID), string(approved.PaymentID), string(noMandate.PaymentID)}, got,
		"newest first")

	over, ok, none := log.Decisions[0], log.Decisions[1], log.Decisions[2]
	assert.Equal(t, "declined", over.Outcome)
	assert.Equal(t, "over_per_tx_cap", over.ReasonCode)
	assert.NotEmpty(t, over.Reason)
	assert.Equal(t, "25", over.Payment.Amount, "a decimal string, never a JSON number")
	assert.Equal(t, "declined", over.Payment.Status)

	assert.Equal(t, "approved", ok.Outcome)
	require.NotNil(t, ok.Mandate)
	assert.Equal(t, 1, ok.Mandate.Version)
	assert.Len(t, ok.Mandate.Digest, 64, "the SHA-256 of the approved bytes, in hex")
	assert.Equal(t, "authorized", ok.Payment.Status)
	assert.Equal(t, "USD", ok.Payment.Asset)
	require.NotNil(t, ok.Payment.PayTo)
	assert.Equal(t, payee, *ok.Payment.PayTo)
	require.NotNil(t, ok.Payment.Category)
	assert.Equal(t, "data_api", *ok.Payment.Category)
	require.NotNil(t, ok.Payment.ResourceURL)
	assert.Equal(t, "funds", ok.Checks[len(ok.Checks)-1].Stage)

	assert.Equal(t, "no_mandate", none.ReasonCode)
	assert.Nil(t, none.Mandate, "there was no mandate to check against")
	last := none.Checks[len(none.Checks)-1]
	assert.Equal(t, "mandate", last.Stage)
	assert.Equal(t, "fail", last.Result, "the last check of a decline is the one that failed")

	declines := f.decisionLog(viewer, "?outcome=declined")
	assert.Len(t, declines.Decisions, 2)

	// One at a time, through the cursor, to the same three.
	var walked []string
	query := "?limit=1"
	for range 5 {
		page := f.decisionLog(viewer, query)
		for _, d := range page.Decisions {
			walked = append(walked, d.Payment.ID)
		}
		if page.NextCursor == nil {
			break
		}
		query = "?limit=1&cursor=" + url.QueryEscape(*page.NextCursor)
	}
	assert.Equal(t, got, walked)
}

// TestDecisionLogRoute_ARevokedAgentKeepsItsHistory: revoking ends what an
// agent can do, not what it did.
func TestDecisionLogRoute_ARevokedAgentKeepsItsHistory(t *testing.T) {
	f := newAPIFixture(t)
	svc := f.paymentService("")
	f.pay(svc, "k1", "5")
	owner := f.session("owner@example.com", "owner", true)

	rec := f.do(http.MethodPost, "/v1/agents/"+string(f.agentID)+"/revoke", "",
		credentials{sessionToken: owner, consoleHeader: true})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assert.Len(t, f.decisionLog(owner, "").Decisions, 1)
}

func TestDecisionLogRoute_Refusals(t *testing.T) {
	f := newAPIFixture(t)
	f.pay(f.paymentService(""), "k1", "5")
	viewer := f.session("viewer@example.com", "viewer", false)
	stranger := func() string {
		other := &apiFixture{t: t, db: f.db, api: f.api}
		other.seedAgent()
		return other.session("stranger@example.com", "owner", true)
	}()
	ours := "/v1/agents/" + string(f.agentID) + "/decisions"
	unknown := "/v1/agents/00000000-0000-0000-0000-000000000000/decisions"
	forged := decisionlog.Cursor{EvaluatedAt: time.Now(), ID: id.DecisionID("not-a-uuid")}.String()

	cases := map[string]struct {
		path, session string
		want          int
	}{
		"stranger reads our log":   {ours, stranger, http.StatusNotFound},
		"unknown agent":            {unknown, viewer, http.StatusNotFound},
		"agent that is not a uuid": {"/v1/agents/buyer/decisions", viewer, http.StatusNotFound},
		"no session":               {ours, "", http.StatusUnauthorized},
		"outcome it cannot hold":   {ours + "?outcome=held", viewer, http.StatusBadRequest},
		"limit of zero":            {ours + "?limit=0", viewer, http.StatusBadRequest},
		"limit past the cap":       {ours + "?limit=101", viewer, http.StatusBadRequest},
		"limit that is not a num":  {ours + "?limit=ten", viewer, http.StatusBadRequest},
		"cursor not issued here":   {ours + "?cursor=" + forged, viewer, http.StatusBadRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := f.do(http.MethodGet, tc.path, "", credentials{sessionToken: tc.session})
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
		})
	}

	// The stranger's answer is the unknown agent's, word for word, so the
	// log cannot be used to learn which agent identifiers are real.
	strange := f.do(http.MethodGet, ours, "", credentials{sessionToken: stranger})
	missing := f.do(http.MethodGet, unknown, "", credentials{sessionToken: stranger})
	assert.Equal(t, withoutRequestID(t, missing), withoutRequestID(t, strange))

	// The agent cannot read its own log: the route is not on its surface.
	rec := f.do(http.MethodGet, ours, "", credentials{agentKey: f.agentKey})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
