package authz_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/modules/authz"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
)

// Test-only assets. The engine compares assets and never interprets them.
const (
	usdc  currency.Asset = "USDC-TEST"
	other currency.Asset = "EURC-TEST"
)

var now = time.Date(2026, 9, 1, 14, 31, 0, 0, time.UTC)

func amt(t *testing.T, s string, a currency.Asset) currency.Amount {
	t.Helper()
	v, err := currency.Parse(s, a)
	require.NoError(t, err)
	return v
}

// base is an agent well inside its limits: 12.40 of 100 spent today, 54 of 200
// this month, 500 in the account.
func base(t *testing.T) authz.Snapshot {
	usd := func(s string) currency.Amount { return amt(t, s, usdc) }
	return authz.Snapshot{
		Mandate: &authz.Rules{
			Asset:       usdc,
			MaxPerTx:    usd("20"),
			MaxPerDay:   usd("100"),
			MaxPerMonth: usd("200"),
			Categories:  []string{"data_api", "inference"},
			ExpiresAt:   time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		DaySpent:       usd("12.40"),
		MonthSpent:     usd("54.00"),
		AccountBalance: usd("500"),
		Now:            now,
	}
}

type decideCase struct {
	name   string
	req    authz.Request
	mutate func(*authz.Snapshot)
	want   authz.ReasonCode
}

// TestDecide covers every reason code once, and the boundary of each limit.
func TestDecide(t *testing.T) {
	for _, tc := range decideCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			snap := base(t)
			if tc.mutate != nil {
				tc.mutate(&snap)
			}

			got := authz.Decide(tc.req, snap)

			assert.Equal(t, tc.want, got.ReasonCode)
			assert.Equal(t, tc.want == authz.ReasonOK, got.Approved())
			assert.NotEmpty(t, got.Reason, "the console always has something to show")
		})
	}
}

// decideCases is shared with the features tests, which replay each one.
func decideCases(t *testing.T) []decideCase {
	usd := func(s string) currency.Amount { return amt(t, s, usdc) }
	req := func(s string) authz.Request { return authz.Request{Amount: usd(s), Category: "data_api"} }

	return []decideCase{
		{name: "within all limits is approved", req: req("0.40"), want: authz.ReasonOK},
		{name: "zero amount is invalid", req: req("0"), want: authz.ReasonInvalidAmount},
		{name: "negative amount is invalid", req: req("-5"), want: authz.ReasonInvalidAmount},
		{
			name:   "frozen or suspended account is declined",
			req:    req("1"),
			mutate: func(s *authz.Snapshot) { s.AccountBlocked = true },
			want:   authz.ReasonAccountBlocked,
		},
		{
			name:   "no mandate is declined",
			req:    req("1"),
			mutate: func(s *authz.Snapshot) { s.Mandate = nil },
			want:   authz.ReasonNoMandate,
		},
		{
			name:   "expired mandate is declined",
			req:    req("1"),
			mutate: func(s *authz.Snapshot) { s.Mandate.ExpiresAt = s.Now.Add(-time.Hour) },
			want:   authz.ReasonMandateExpired,
		},
		{
			name:   "mandate expires at its expiry instant",
			req:    req("1"),
			mutate: func(s *authz.Snapshot) { s.Mandate.ExpiresAt = s.Now },
			want:   authz.ReasonMandateExpired,
		},
		{
			name:   "one second before expiry is approved",
			req:    req("1"),
			mutate: func(s *authz.Snapshot) { s.Mandate.ExpiresAt = s.Now.Add(time.Second) },
			want:   authz.ReasonOK,
		},
		{
			name: "payment in another asset is declined, not converted",
			req:  authz.Request{Amount: amt(t, "1", other), Category: "data_api"},
			want: authz.ReasonAssetNotPermitted,
		},
		{
			name: "category not permitted is declined",
			req:  authz.Request{Amount: usd("1"), Category: "gambling"},
			want: authz.ReasonCategoryNotPermitted,
		},
		{
			name: "missing category is declined",
			req:  authz.Request{Amount: usd("1")},
			want: authz.ReasonCategoryNotPermitted,
		},
		{
			name:   "payee off the allowlist is declined",
			req:    authz.Request{Amount: usd("1"), Category: "data_api", PayTo: "stranger"},
			mutate: func(s *authz.Snapshot) { s.Mandate.PayTo = []string{"known"} },
			want:   authz.ReasonPayeeNotPermitted,
		},
		{
			name:   "payee on the allowlist is approved",
			req:    authz.Request{Amount: usd("1"), Category: "data_api", PayTo: "known"},
			mutate: func(s *authz.Snapshot) { s.Mandate.PayTo = []string{"known"} },
			want:   authz.ReasonOK,
		},
		{
			name: "empty allowlist permits any payee",
			req:  authz.Request{Amount: usd("1"), Category: "data_api", PayTo: "anyone"},
			want: authz.ReasonOK,
		},
		{
			// The whitepaper's own dashboard example, §5.1
			name: "over per-transaction cap is declined",
			req:  req("85"),
			want: authz.ReasonOverPerTxCap,
		},
		{name: "exactly at the per-transaction cap is approved", req: req("20"), want: authz.ReasonOK},
		{
			name:   "would breach daily cap is declined",
			req:    req("19"),
			mutate: func(s *authz.Snapshot) { s.DaySpent = usd("95") },
			want:   authz.ReasonOverDailyCap,
		},
		{
			name:   "reaching the daily cap exactly is approved",
			req:    req("5"),
			mutate: func(s *authz.Snapshot) { s.DaySpent = usd("95") },
			want:   authz.ReasonOK,
		},
		{
			name:   "would breach monthly cap is declined",
			req:    req("10"),
			mutate: func(s *authz.Snapshot) { s.MonthSpent = usd("195") },
			want:   authz.ReasonOverMonthlyCap,
		},
		{
			name:   "reaching the monthly cap exactly is approved",
			req:    req("5"),
			mutate: func(s *authz.Snapshot) { s.MonthSpent = usd("195") },
			want:   authz.ReasonOK,
		},
		{
			name:   "insufficient account balance is declined",
			req:    req("10"),
			mutate: func(s *authz.Snapshot) { s.AccountBalance = usd("5") },
			want:   authz.ReasonInsufficientFunds,
		},
		{
			name:   "spending the whole balance is approved",
			req:    req("10"),
			mutate: func(s *authz.Snapshot) { s.AccountBalance = usd("10") },
			want:   authz.ReasonOK,
		},
	}
}

// TestDecide_FailsClosedOnMismatchedInputs: an input in the wrong asset is a
// bug in whatever built the snapshot. currency refuses the comparison, and the
// engine must decline rather than read the refusal as "not over the cap".
func TestDecide_FailsClosedOnMismatchedInputs(t *testing.T) {
	req := authz.Request{Amount: amt(t, "1", usdc), Category: "data_api"}

	for name, mutate := range mismatchCases(t) {
		t.Run(name, func(t *testing.T) {
			snap := base(t)
			mutate(&snap)

			got := authz.Decide(req, snap)

			assert.False(t, got.Approved())
			assert.Equal(t, authz.ReasonProcessingError, got.ReasonCode)
		})
	}
}

func mismatchCases(t *testing.T) map[string]func(*authz.Snapshot) {
	return map[string]func(*authz.Snapshot){
		"per-transaction cap": func(s *authz.Snapshot) { s.Mandate.MaxPerTx = amt(t, "20", other) },
		"daily cap":           func(s *authz.Snapshot) { s.Mandate.MaxPerDay = amt(t, "100", other) },
		"monthly cap":         func(s *authz.Snapshot) { s.Mandate.MaxPerMonth = amt(t, "200", other) },
		"daily spend":         func(s *authz.Snapshot) { s.DaySpent = amt(t, "0", other) },
		"monthly spend":       func(s *authz.Snapshot) { s.MonthSpent = amt(t, "0", other) },
		"balance":             func(s *authz.Snapshot) { s.AccountBalance = amt(t, "500", other) },

		// The zero Amount has no asset. A counter with no spend yet must be
		// currency.Zero of the mandate's asset.
		"counter left as the zero Amount": func(s *authz.Snapshot) { s.DaySpent = currency.Amount{} },
		"balance left as the zero Amount": func(s *authz.Snapshot) { s.AccountBalance = currency.Amount{} },
	}
}

// TestDecide_FirstFailureWins pins the published order: a payment that
// breaks several rules reports the earliest one.
func TestDecide_FirstFailureWins(t *testing.T) {
	usd := func(s string) currency.Amount { return amt(t, s, usdc) }

	// Breaks everything after the account check: expired, wrong category,
	// over every cap and over the balance.
	req := authz.Request{Amount: usd("1000"), Category: "gambling", PayTo: "stranger"}
	breakAll := func(s *authz.Snapshot) {
		s.Mandate.ExpiresAt = s.Now.Add(-time.Hour)
		s.Mandate.PayTo = []string{"known"}
		s.AccountBalance = usd("1")
	}

	snap := base(t)
	breakAll(&snap)
	assert.Equal(t, authz.ReasonMandateExpired, authz.Decide(req, snap).ReasonCode)

	snap.AccountBlocked = true
	assert.Equal(t, authz.ReasonAccountBlocked, authz.Decide(req, snap).ReasonCode)

	snap = base(t)
	breakAll(&snap)
	snap.Mandate.ExpiresAt = now.Add(time.Hour)
	assert.Equal(t, authz.ReasonCategoryNotPermitted, authz.Decide(req, snap).ReasonCode)

	req.Category = "data_api"
	assert.Equal(t, authz.ReasonPayeeNotPermitted, authz.Decide(req, snap).ReasonCode)

	req.PayTo = "known"
	assert.Equal(t, authz.ReasonOverPerTxCap, authz.Decide(req, snap).ReasonCode)

	// Within the per-transaction cap but over the balance: the caps pass and
	// funds are the last thing checked.
	req.Amount = usd("10")
	assert.Equal(t, authz.ReasonInsufficientFunds, authz.Decide(req, snap).ReasonCode)
}

// TestDecide_RecordsTheStagesThatRan: the checks become decision_check rows,
// so they must show what was evaluated and nothing more.
func TestDecide_RecordsTheStagesThatRan(t *testing.T) {
	usd := func(s string) currency.Amount { return amt(t, s, usdc) }

	t.Run("approval runs every stage and skips the unbuilt ones", func(t *testing.T) {
		got := authz.Decide(authz.Request{Amount: usd("1"), Category: "data_api"}, base(t))

		assert.Equal(t, []authz.Check{
			{Stage: authz.StageRequest, Result: authz.Pass},
			{Stage: authz.StageAccount, Result: authz.Pass},
			{Stage: authz.StageMandate, Result: authz.Pass},
			{Stage: authz.StageCounterparty, Result: authz.Skipped},
			{Stage: authz.StageIntent, Result: authz.Skipped},
			{Stage: authz.StageBehaviour, Result: authz.Skipped},
			{Stage: authz.StageFunds, Result: authz.Pass},
		}, got.Checks)
	})

	t.Run("decline stops at the failing stage", func(t *testing.T) {
		got := authz.Decide(authz.Request{Amount: usd("85"), Category: "data_api"}, base(t))

		assert.Equal(t, []authz.Check{
			{Stage: authz.StageRequest, Result: authz.Pass},
			{Stage: authz.StageAccount, Result: authz.Pass},
			{Stage: authz.StageMandate, Result: authz.Fail},
		}, got.Checks)
	})
}

func TestDecide_BoundsAgentInputInReason(t *testing.T) {
	long := string(make([]byte, 10_000))
	got := authz.Decide(authz.Request{Amount: amt(t, "1", usdc), Category: long}, base(t))

	require.Equal(t, authz.ReasonCategoryNotPermitted, got.ReasonCode)
	assert.Less(t, len(got.Reason), 200)
}
