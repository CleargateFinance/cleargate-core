package payment

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/CleargateFinance/cleargate-core/internal/modules/authz"
	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
)

func amount(t *testing.T, s string) currency.Amount {
	t.Helper()
	a, err := currency.Parse(s, currency.USD)
	require.NoError(t, err)
	return a
}

func validRequest(t *testing.T) Request {
	return Request{
		AccountID: "acct", AgentID: "agent", IdempotencyKey: "k1",
		Amount: amount(t, "0.40"), Category: "data_api", PayTo: "payee",
		Signals: authz.Signals{ResourceURL: "https://api.example.com/v1/quote", Purpose: "price lookup"},
	}
}

func TestRequest_ValidateRefusesWhatCannotBeRecorded(t *testing.T) {
	cases := map[string]func(*Request){
		"no key":              func(r *Request) { r.IdempotencyKey = "" },
		"long key":            func(r *Request) { r.IdempotencyKey = strings.Repeat("k", 256) },
		"amount has no asset": func(r *Request) { r.Amount = currency.Amount{} },
		"too many decimals":   func(r *Request) { r.Amount = amount(t, "0.0000000000000000001") },
		"too large":           func(r *Request) { r.Amount = amount(t, "100000000000000000000") },
		"long category":       func(r *Request) { r.Category = strings.Repeat("c", 257) },
		"long payee":          func(r *Request) { r.PayTo = strings.Repeat("p", 257) },
		"long resource url":   func(r *Request) { r.Signals.ResourceURL = strings.Repeat("u", 2049) },
		"long purpose":        func(r *Request) { r.Signals.Purpose = strings.Repeat("p", 513) },
		"long task":           func(r *Request) { r.Signals.Task = strings.Repeat("t", 1025) },
		"long session id":     func(r *Request) { r.Signals.SessionID = strings.Repeat("s", 129) },
		"unknown url source":  func(r *Request) { r.Signals.URLSource = "inbox" },
		"nul in purpose":      func(r *Request) { r.Signals.Purpose = "a\x00b" },
		"nul in key":          func(r *Request) { r.IdempotencyKey = "k\x00" },
		"invalid utf-8":       func(r *Request) { r.Category = "data\xff" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r := validRequest(t)
			mutate(&r)
			assert.Equal(t, apperr.KindInvalid, apperr.KindOf(r.Validate()))
		})
	}
}

// TestRequest_ValidateLeavesDecisionsToTheEngine: a zero or negative amount
// is the invalid_amount decline, recorded like any other, not a bad request.
func TestRequest_ValidateLeavesDecisionsToTheEngine(t *testing.T) {
	for _, s := range []string{"0", "-1", "0.000000000000000001", "1.000000000000000000000"} {
		r := validRequest(t)
		r.Amount = amount(t, s)
		assert.NoError(t, r.Validate(), s)
	}
}

func TestRequest_HashChangesWithEveryField(t *testing.T) {
	base := validRequest(t).hash()
	novel := 1
	mutations := map[string]func(*Request){
		"agent":    func(r *Request) { r.AgentID = "other" },
		"amount":   func(r *Request) { r.Amount = amount(t, "0.41") },
		"asset":    func(r *Request) { r.Amount, _ = currency.Parse("0.40", "EUR") },
		"category": func(r *Request) { r.Category = "inference" },
		"payee":    func(r *Request) { r.PayTo = "someone else" },
		"url":      func(r *Request) { r.Signals.ResourceURL = "https://evil.example" },
		"source":   func(r *Request) { r.Signals.URLSource = authz.URLSourceFetched },
		"purpose":  func(r *Request) { r.Signals.Purpose = "other" },
		"task":     func(r *Request) { r.Signals.Task = "other" },
		"session":  func(r *Request) { r.Signals.SessionID = "s2" },
		"novel":    func(r *Request) { r.Signals.SessionNovelCounterparties = &novel },
	}
	for name, mutate := range mutations {
		r := validRequest(t)
		mutate(&r)
		assert.NotEqual(t, base, r.hash(), name)
	}
}

// TestRequest_HashIgnoresTheKeyAndTheAmountsSpelling: the key is what the
// hash is stored under, and 0.40 and 0.4 are the same price.
func TestRequest_HashIgnoresTheKeyAndTheAmountsSpelling(t *testing.T) {
	a, b := validRequest(t), validRequest(t)
	b.IdempotencyKey = "k2"
	b.Amount = amount(t, "0.4")

	assert.Equal(t, a.hash(), b.hash())
}

func TestResult_RoundTripsThroughStorage(t *testing.T) {
	want := Result{
		PaymentID: "p1", Status: StatusDeclined, Outcome: authz.Declined,
		ReasonCode: authz.ReasonOverDailyCap, Reason: "over the daily cap",
		DecidedAt: time.Date(2026, 11, 3, 9, 30, 0, 123000, time.UTC),
	}
	raw, err := encodeResult(want)
	require.NoError(t, err)

	got, err := decodeResult(raw)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	_, err = decodeResult([]byte(`{"v":2}`))
	assert.Equal(t, apperr.KindInternal, apperr.KindOf(err))
}
