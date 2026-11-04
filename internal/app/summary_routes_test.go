//go:build integration

package app

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive Task 6.2's account summary: payments go through the real
// authorize path, and the console reads back the balance, each agent against
// its caps, and the blocked count.

type amountJSON struct {
	Amount string `json:"amount"`
	Asset  string `json:"asset"`
}

type declinesJSON struct {
	Count   int          `json:"count"`
	Amounts []amountJSON `json:"amounts"`
}

type summaryJSON struct {
	Account struct {
		ID       string     `json:"id"`
		Role     string     `json:"role"`
		FrozenAt *time.Time `json:"frozen_at"`
	} `json:"account"`
	Balance    amountJSON `json:"balance"`
	DayStart   time.Time  `json:"day_start"`
	MonthStart time.Time  `json:"month_start"`
	Agents     []struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Mandate *struct {
			Version     int    `json:"version"`
			Asset       string `json:"asset"`
			MaxPerTx    string `json:"max_per_tx"`
			MaxPerDay   string `json:"max_per_day"`
			MaxPerMonth string `json:"max_per_month"`
		} `json:"mandate"`
		SpentToday        string       `json:"spent_today"`
		SpentThisMonth    string       `json:"spent_this_month"`
		DeclinedThisMonth declinesJSON `json:"declined_this_month"`
	} `json:"agents"`
	DeclinedThisMonth declinesJSON `json:"declined_this_month"`
}

func (f *apiFixture) summary(session string) summaryJSON {
	f.t.Helper()
	rec := f.do(http.MethodGet, "/v1/accounts/"+string(f.accountID)+"/summary", "", credentials{sessionToken: session})
	require.Equal(f.t, http.StatusOK, rec.Code, rec.Body.String())
	return decode[summaryJSON](f.t, rec.Body.Bytes())
}

// TestSummaryRoute_BalanceCapsAndTheBlockedCount: one payment declined for
// want of a mandate, one approved under it, one over its per-payment cap and
// one for an invalid amount. A viewer sees all of it.
func TestSummaryRoute_BalanceCapsAndTheBlockedCount(t *testing.T) {
	f := newAPIFixture(t)
	svc := f.paymentService("100")
	owner := f.session("owner@example.com", "owner", true)
	viewer := f.session("viewer@example.com", "viewer", false)

	f.pay(svc, "k1", "5")
	rec := f.do(http.MethodPut, "/v1/agents/"+string(f.agentID)+"/mandate", rulesJSON,
		credentials{sessionToken: owner, consoleHeader: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	f.pay(svc, "k2", "5")
	f.pay(svc, "k3", "25")
	f.pay(svc, "k4", "-1")

	got := f.summary(viewer)

	assert.Equal(t, string(f.accountID), got.Account.ID)
	assert.Equal(t, "viewer", got.Account.Role)
	assert.Nil(t, got.Account.FrozenAt)
	assert.Equal(t, amountJSON{"95", "USD"}, got.Balance, "a decimal string at par, never a JSON number")

	now := time.Now().UTC()
	assert.Equal(t, time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), got.DayStart.UTC())
	assert.Equal(t, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC), got.MonthStart.UTC())

	require.Len(t, got.Agents, 1)
	a := got.Agents[0]
	assert.Equal(t, string(f.agentID), a.ID)
	assert.Equal(t, "active", a.Status)
	require.NotNil(t, a.Mandate)
	assert.Equal(t, 1, a.Mandate.Version)
	assert.Equal(t, "USD", a.Mandate.Asset)
	assert.Equal(t, []string{"20", "100", "200"}, []string{a.Mandate.MaxPerTx, a.Mandate.MaxPerDay, a.Mandate.MaxPerMonth})
	assert.Equal(t, "5", a.SpentToday, "only the approval counts against the caps")
	assert.Equal(t, "5", a.SpentThisMonth)

	want := declinesJSON{Count: 3, Amounts: []amountJSON{{"30", "USD"}}}
	assert.Equal(t, want, a.DeclinedThisMonth, "the invalid amount is counted and adds nothing")
	assert.Equal(t, want, got.DeclinedThisMonth)
}

// TestSummaryRoute_AFreshAccountShowsZeroesNotGaps: before any money or any
// mandate, every figure is present and zero, and the lists are empty rather
// than null, so the console renders the empty state without special cases.
func TestSummaryRoute_AFreshAccountShowsZeroesNotGaps(t *testing.T) {
	f := newAPIFixture(t)
	viewer := f.session("viewer@example.com", "viewer", false)

	rec := f.do(http.MethodGet, "/v1/accounts/"+string(f.accountID)+"/summary", "", credentials{sessionToken: viewer})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"declined_this_month":{"count":0,"amounts":[]}`)
	assert.Contains(t, rec.Body.String(), `"mandate":null`)

	got := decode[summaryJSON](t, rec.Body.Bytes())
	assert.Equal(t, amountJSON{"0", "USD"}, got.Balance)
	require.Len(t, got.Agents, 1)
	assert.Equal(t, "0", got.Agents[0].SpentToday)
}

// TestSummaryRoute_ShowsTheFreeze: the summary is where an owner confirms the
// freeze took.
func TestSummaryRoute_ShowsTheFreeze(t *testing.T) {
	f := newAPIFixture(t)
	owner := f.session("owner@example.com", "owner", true)

	rec := f.do(http.MethodPost, "/v1/accounts/"+string(f.accountID)+"/freeze", "",
		credentials{sessionToken: owner, consoleHeader: true})
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assert.NotNil(t, f.summary(owner).Account.FrozenAt)
}

func TestSummaryRoute_Refusals(t *testing.T) {
	f := newAPIFixture(t)
	stranger := func() string {
		other := &apiFixture{t: t, db: f.db, api: f.api}
		other.seedAgent()
		return other.session("stranger@example.com", "owner", true)
	}()
	ours := "/v1/accounts/" + string(f.accountID) + "/summary"
	unknown := "/v1/accounts/00000000-0000-0000-0000-000000000000/summary"

	cases := map[string]struct {
		path, session string
		want          int
	}{
		"stranger reads our summary": {ours, stranger, http.StatusForbidden},
		"unknown account":            {unknown, stranger, http.StatusForbidden},
		"account that is not a uuid": {"/v1/accounts/acme/summary", stranger, http.StatusForbidden},
		"no session":                 {ours, "", http.StatusUnauthorized},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := f.do(http.MethodGet, tc.path, "", credentials{sessionToken: tc.session})
			assert.Equal(t, tc.want, rec.Code, rec.Body.String())
		})
	}

	// One answer for all three, word for word, so the route cannot be used to
	// learn which account identifiers are real.
	strange := withoutRequestID(t, f.do(http.MethodGet, ours, "", credentials{sessionToken: stranger}))
	assert.Equal(t, strange, withoutRequestID(t, f.do(http.MethodGet, unknown, "", credentials{sessionToken: stranger})))
	assert.Equal(t, strange, withoutRequestID(t, f.do(http.MethodGet, "/v1/accounts/acme/summary", "", credentials{sessionToken: stranger})))

	// An agent cannot read the balance it draws on: the route is not on its
	// surface.
	rec := f.do(http.MethodGet, ours, "", credentials{agentKey: f.agentKey})
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// TestSummaryRoute_ANameForEachAccountAPersonBelongsTo: somebody in two
// accounts reads each one's summary as that account, with the role they hold
// there, not whichever they joined first.
func TestSummaryRoute_ANameForEachAccountAPersonBelongsTo(t *testing.T) {
	f := newAPIFixture(t)
	other := &apiFixture{t: t, db: f.db, api: f.api}
	other.seedAgent()
	both := other.session("both@example.com", "owner", false)
	_, err := f.db.Exec(t.Context(), `
		INSERT INTO account_membership (account_id, user_id, role)
		SELECT $1, id, 'viewer' FROM app_user WHERE email = 'both@example.com'`, string(f.accountID))
	require.NoError(t, err)

	got := f.summary(both)

	assert.Equal(t, string(f.accountID), got.Account.ID)
	assert.Equal(t, "viewer", got.Account.Role)
}

// TestSummaryRoute_AnInvalidAmountIsCountedButAddsNothing: a decline that
// asked for nothing is still a blocked payment, and still not money at risk.
func TestSummaryRoute_AnInvalidAmountIsCountedButAddsNothing(t *testing.T) {
	f := newAPIFixture(t)
	f.pay(f.paymentService(""), "k1", "-1")
	viewer := f.session("viewer@example.com", "viewer", false)

	got := f.summary(viewer)

	want := declinesJSON{Count: 1, Amounts: []amountJSON{}}
	assert.Equal(t, want, got.Agents[0].DeclinedThisMonth)
	assert.Equal(t, want, got.DeclinedThisMonth)
}
