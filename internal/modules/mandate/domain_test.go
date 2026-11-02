package mandate

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

const testAsset = currency.USD

var testNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

func amount(t *testing.T, s string) currency.Amount {
	t.Helper()
	a, err := currency.Parse(s, testAsset)
	require.NoError(t, err)
	return a
}

// validRules is a mandate the whitepaper would recognise: $20 a call, $100 a
// day, $200 a month, for data and inference APIs (§5.1).
func validRules(t *testing.T) Rules {
	t.Helper()
	return Rules{
		Asset:       testAsset,
		MaxPerTx:    amount(t, "20"),
		MaxPerDay:   amount(t, "100"),
		MaxPerMonth: amount(t, "200"),
		Categories:  []string{"inference", "data_api"},
		ExpiresAt:   testNow.Add(90 * 24 * time.Hour),
	}
}

func TestRules_Validate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Rules)
		wantErr string
	}{
		{name: "valid rules pass"},
		{
			name:    "missing asset",
			mutate:  func(r *Rules) { r.Asset = "" },
			wantErr: "asset is required",
		},
		{
			name: "cap in another asset",
			mutate: func(r *Rules) {
				other, _ := currency.Parse("20", "USDC-SOL")
				r.MaxPerTx = other
			},
			wantErr: "per-transaction cap must be in USD",
		},
		{
			name:    "zero cap",
			mutate:  func(r *Rules) { r.MaxPerTx = currency.Zero(testAsset) },
			wantErr: "per-transaction cap must be positive",
		},
		{
			name:    "negative cap",
			mutate:  func(r *Rules) { r.MaxPerDay = amount(t, "-1") },
			wantErr: "daily cap must be positive",
		},
		{
			name:    "cap finer than storage",
			mutate:  func(r *Rules) { r.MaxPerTx = amount(t, "1.0000000000000000001") },
			wantErr: "too many decimal places",
		},
		{
			// Trailing zeros are not precision: this is exactly 1.
			name:   "trailing zeros beyond storage are fine",
			mutate: func(r *Rules) { r.MaxPerTx = amount(t, "1.0000000000000000000000") },
		},
		{
			name:    "per-transaction above daily",
			mutate:  func(r *Rules) { r.MaxPerTx = amount(t, "150") },
			wantErr: "per-transaction cap cannot exceed the daily cap",
		},
		{
			name:    "daily above monthly",
			mutate:  func(r *Rules) { r.MaxPerDay = amount(t, "250") },
			wantErr: "daily cap cannot exceed the monthly cap",
		},
		{
			name:    "no categories",
			mutate:  func(r *Rules) { r.Categories = nil },
			wantErr: "at least one category",
		},
		{
			name:    "category that is not a slug",
			mutate:  func(r *Rules) { r.Categories = []string{"Data API"} },
			wantErr: "must be a lower-case slug",
		},
		{
			name: "too many categories",
			mutate: func(r *Rules) {
				r.Categories = nil
				for i := 0; i <= MaxCategories; i++ {
					r.Categories = append(r.Categories, "c"+strings.Repeat("x", i))
				}
			},
			wantErr: "too many categories",
		},
		{
			name:    "payee with whitespace",
			mutate:  func(r *Rules) { r.PayTo = []string{"So1ana Address"} },
			wantErr: "is not an address",
		},
		{
			name:    "payee outside printable ascii",
			mutate:  func(r *Rules) { r.PayTo = []string{"0xabéc"} },
			wantErr: "is not an address",
		},
		{
			name:    "payee too long",
			mutate:  func(r *Rules) { r.PayTo = []string{strings.Repeat("a", MaxPayeeLength+1)} },
			wantErr: "is not an address",
		},
		{
			name:    "no expiry",
			mutate:  func(r *Rules) { r.ExpiresAt = time.Time{} },
			wantErr: "expiry is required",
		},
		{
			name:    "expiry in the past",
			mutate:  func(r *Rules) { r.ExpiresAt = testNow.Add(-time.Minute) },
			wantErr: "expiry must be in the future",
		},
		{
			name:    "expiry exactly now",
			mutate:  func(r *Rules) { r.ExpiresAt = testNow },
			wantErr: "expiry must be in the future",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRules(t)
			if tc.mutate != nil {
				tc.mutate(&r)
			}
			err := r.normalised().Validate(testNow)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestCanonicalise_IsStableAcrossMarshals is the property Task 3.1's
// signatures depend on: the same rules always produce the same bytes.
func TestCanonicalise_IsStableAcrossMarshals(t *testing.T) {
	agentID := id.AgentID("6f1c3a52-6a3e-4c55-9a51-1c0d7a1b2e33")
	first, err := Canonicalise(agentID, 3, validRules(t))
	require.NoError(t, err)

	for i := 0; i < 100; i++ {
		again, err := Canonicalise(agentID, 3, validRules(t))
		require.NoError(t, err)
		require.Equal(t, first, again)
	}
}

// TestCanonicalise_SameMeaningSameBytes covers the ways one set of rules can
// be written differently: list order, duplicates, padding, a time zone, a
// sub-second expiry and trailing zeros on an amount.
func TestCanonicalise_SameMeaningSameBytes(t *testing.T) {
	agentID := id.AgentID("6f1c3a52-6a3e-4c55-9a51-1c0d7a1b2e33")
	want, err := Canonicalise(agentID, 1, validRules(t))
	require.NoError(t, err)

	r := validRules(t)
	r.Categories = []string{" data_api", "inference", "data_api"}
	r.MaxPerTx = amount(t, "20.000")
	r.ExpiresAt = r.ExpiresAt.In(time.FixedZone("CET", 3600)).Add(400 * time.Millisecond)

	got, err := Canonicalise(agentID, 1, r)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(got))
}

// TestCanonicalise_ExactEncoding pins the bytes. Keys sorted, no whitespace,
// no HTML escaping, amounts as minimal decimal strings: the JCS form for this
// restricted alphabet. A change here invalidates every signature made under
// format 1, so it must come with a new PayloadFormat instead.
func TestCanonicalise_ExactEncoding(t *testing.T) {
	r := validRules(t)
	r.MaxPerTx = amount(t, "0.40")
	r.PayTo = []string{"0xB&<>", "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU"}
	r.ExpiresAt = time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	got, err := Canonicalise("6f1c3a52-6a3e-4c55-9a51-1c0d7a1b2e33", 2, r)
	require.NoError(t, err)

	want := `{"agent_id":"6f1c3a52-6a3e-4c55-9a51-1c0d7a1b2e33","asset":"USD",` +
		`"categories":["data_api","inference"],"expires_at":"2027-01-01T00:00:00Z",` +
		`"format":1,"max_per_day":"100","max_per_month":"200","max_per_tx":"0.4",` +
		`"pay_to":["0xB&<>","7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU"],"version":2}`
	assert.Equal(t, want, string(got))
}

func TestParsePayload_RoundTrips(t *testing.T) {
	agentID := id.AgentID("6f1c3a52-6a3e-4c55-9a51-1c0d7a1b2e33")
	r := validRules(t).normalised()

	b, err := Canonicalise(agentID, 4, r)
	require.NoError(t, err)

	gotAgent, gotVersion, got, err := parsePayload(b)
	require.NoError(t, err)
	assert.Equal(t, agentID, gotAgent)
	assert.Equal(t, 4, gotVersion)
	assert.True(t, r.MaxPerTx.Equal(got.MaxPerTx))
	assert.True(t, r.MaxPerDay.Equal(got.MaxPerDay))
	assert.True(t, r.MaxPerMonth.Equal(got.MaxPerMonth))
	assert.Equal(t, r.Categories, got.Categories)
	assert.Empty(t, got.PayTo)
	assert.True(t, r.ExpiresAt.Equal(got.ExpiresAt))
}

// TestParsePayload_RejectsWhatItCannotEnforce: a field this build does not
// know may be a limit, and enforcing the rest would grant more than was
// approved.
func TestParsePayload_RejectsWhatItCannotEnforce(t *testing.T) {
	b, err := Canonicalise("6f1c3a52-6a3e-4c55-9a51-1c0d7a1b2e33", 1, validRules(t))
	require.NoError(t, err)

	cases := map[string]string{
		"unknown field":  strings.Replace(string(b), `"format":1`, `"format":1,"review_above":"5"`, 1),
		"newer format":   strings.Replace(string(b), `"format":1`, `"format":2`, 1),
		"trailing value": string(b) + `{}`,
		"amount number":  strings.Replace(string(b), `"max_per_day":"100"`, `"max_per_day":100`, 1),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := parsePayload([]byte(payload))
			assert.Error(t, err)
		})
	}
}
