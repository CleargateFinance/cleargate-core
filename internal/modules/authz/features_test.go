package authz_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/modules/authz"
	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
)

func signals() authz.Signals {
	novel := 2
	return authz.Signals{
		ResourceURL:                "https://api.example.com/v1/quote",
		URLSource:                  authz.URLSourceFetched,
		Purpose:                    "price lookup",
		Task:                       "compile a market report",
		SessionID:                  "sess-1",
		SessionNovelCounterparties: &novel,
	}
}

// roundTrip stores features as decision.features would hold them and reads
// them back.
func roundTrip(t *testing.T, f authz.Features) authz.Features {
	t.Helper()
	raw, err := json.Marshal(f)
	require.NoError(t, err)
	var back authz.Features
	require.NoError(t, json.Unmarshal(raw, &back))
	return back
}

// TestFeatures_CapturesAllDecisionInputs: the stored snapshot alone is enough
// to reach the same decision again, for every outcome the engine can give,
// including the declines caused by inputs that disagree with each other.
func TestFeatures_CapturesAllDecisionInputs(t *testing.T) {
	type replayCase struct {
		name   string
		req    authz.Request
		mutate func(*authz.Snapshot)
	}
	var cases []replayCase
	for _, tc := range decideCases(t) {
		cases = append(cases, replayCase{tc.name, tc.req, tc.mutate})
	}
	for name, mutate := range mismatchCases(t) {
		cases = append(cases, replayCase{"mismatch: " + name, authz.Request{Amount: amt(t, "1", usdc), Category: "data_api"}, mutate})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := base(t)
			if tc.mutate != nil {
				tc.mutate(&snap)
			}
			req := tc.req
			req.Signals = signals()

			original := authz.Decide(req, snap)

			replayReq, replaySnap, err := roundTrip(t, original.Features).Inputs()
			require.NoError(t, err)

			assert.Equal(t, original, authz.Decide(replayReq, replaySnap))
		})
	}
}

// TestFeatures_EncodingIsPinned: decision.features is read for years, so a
// change to its shape must be deliberate and bump FeaturesVersion. This
// fails on any accidental one.
func TestFeatures_EncodingIsPinned(t *testing.T) {
	req := authz.Request{Amount: amt(t, "0.40", usdc), Category: "data_api", Signals: signals()}

	raw, err := json.Marshal(authz.Decide(req, base(t)).Features)
	require.NoError(t, err)

	assert.Equal(t, `{"v":1,"now":"2026-09-01T14:31:00Z",`+
		`"request":{"amount":{"value":"0.4","asset":"USDC-TEST"},"category":"data_api","pay_to":"",`+
		`"resource_url":"https://api.example.com/v1/quote","url_source":"fetched","purpose":"price lookup",`+
		`"task":"compile a market report","session_id":"sess-1","session_novel_counterparties":2},`+
		`"mandate":{"asset":"USDC-TEST","max_per_tx":{"value":"20","asset":"USDC-TEST"},`+
		`"max_per_day":{"value":"100","asset":"USDC-TEST"},"max_per_month":{"value":"200","asset":"USDC-TEST"},`+
		`"categories":["data_api","inference"],"pay_to":null,"expires_at":"2027-01-01T00:00:00Z"},`+
		`"account_blocked":false,"day_spent":{"value":"12.4","asset":"USDC-TEST"},`+
		`"month_spent":{"value":"54","asset":"USDC-TEST"},"account_balance":{"value":"500","asset":"USDC-TEST"}}`,
		string(raw))
}

func TestFeatures_UnreportedURLSourceIsUnknown(t *testing.T) {
	req := authz.Request{Amount: amt(t, "1", usdc), Category: "data_api"}

	f := authz.Decide(req, base(t)).Features

	assert.Equal(t, authz.URLSourceUnknown, f.Request.URLSource)
	assert.Nil(t, f.Request.SessionNovelCounterparties, "not reported is not zero")
}

func TestFeatures_RecordsMissingMandateAsNull(t *testing.T) {
	snap := base(t)
	snap.Mandate = nil

	d := authz.Decide(authz.Request{Amount: amt(t, "1", usdc), Category: "data_api"}, snap)
	require.Equal(t, authz.ReasonNoMandate, d.ReasonCode)

	raw, err := json.Marshal(d.Features)
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"mandate":null`)
}

// TestFeatures_DoNotShareInputSlices: the record must say what was evaluated
// even if the caller reuses the mandate's slices afterwards.
func TestFeatures_DoNotShareInputSlices(t *testing.T) {
	snap := base(t)
	snap.Mandate.PayTo = []string{"known"}
	novel := 3
	req := authz.Request{Amount: amt(t, "1", usdc), Category: "data_api", PayTo: "known"}
	req.Signals.SessionNovelCounterparties = &novel

	f := authz.Decide(req, snap).Features

	snap.Mandate.Categories[0] = "gambling"
	snap.Mandate.PayTo[0] = "stranger"
	novel = 99

	assert.Equal(t, []string{"data_api", "inference"}, f.Mandate.Categories)
	assert.Equal(t, []string{"known"}, f.Mandate.PayTo)
	assert.Equal(t, 3, *f.Request.SessionNovelCounterparties)
}

func TestFeatures_InputsRefusesWhatItCannotRead(t *testing.T) {
	good := authz.Decide(authz.Request{Amount: amt(t, "1", usdc), Category: "data_api"}, base(t)).Features

	cases := map[string]func(*authz.Features){
		"unknown version":       func(f *authz.Features) { f.V = 99 },
		"unparseable amount":    func(f *authz.Features) { f.DaySpent.Value = "twelve" },
		"value without asset":   func(f *authz.Features) { f.AccountBalance.Asset = "" },
		"unparseable mandate":   func(f *authz.Features) { f.Mandate.MaxPerTx.Value = "" },
		"unparseable requested": func(f *authz.Features) { f.Request.Amount.Value = "1e" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := roundTrip(t, good)
			mutate(&f)

			_, _, err := f.Inputs()
			assert.Equal(t, apperr.KindInternal, apperr.KindOf(err))
		})
	}
}
