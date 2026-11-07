package payment

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/shopspring/decimal"

	"github.com/CleargateFinance/cleargate-core/internal/modules/authz"
	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// KeyRetention is how long an idempotency key is remembered. A retry inside
// it replays the first result; one after it is a new payment, which is why
// the SDK makes one key per logical payment and reuses it only for retries of
// that payment (Task 5.2). The API docs publish this figure.
const KeyRetention = 24 * time.Hour

// Length bounds, in characters. The first four the database holds too
// (migration 000007), checked here so an over-long field fails as a bad
// request naming the field, not as a constraint violation. The signals have
// no column of their own, but every decision stores them, so their bound is
// what keeps a decision record small (Task 6.2 publishes all of these).
const (
	maxKeyLength         = 255
	maxCategoryLength    = 256
	maxPayToLength       = 256
	maxResourceURLLength = 2048
	maxPurposeLength     = 512
	maxTaskLength        = 1024
	maxSessionIDLength   = 128
)

// payment.amount is numeric(38, 18). A value it cannot hold exactly would be
// rounded on insert, and the row would disagree with the decision that judged
// it, so such a value is refused before anything is decided.
var (
	maxAmountScale int32 = 18
	maxAmount            = decimal.New(1, 20)
)

// Request is one payment an agent asks for.
type Request struct {
	// AccountID and AgentID come from the authenticated credential, never
	// from the request body.
	AccountID id.AccountID
	AgentID   id.AgentID

	// IdempotencyKey identifies this logical payment across retries (§8.4).
	IdempotencyKey string

	// Amount is the price quoted, in the unit of account.
	Amount   currency.Amount
	Category string
	PayTo    string

	// Signals are the agent's report about the payment, recorded with the
	// decision (Task 4.2). Signals.ResourceURL is also kept on the payment.
	Signals authz.Signals

	// Terms is the rest of a merchant's requirement that makes it this payment,
	// such as its order reference. Nothing decides on it; it is in the hash, so
	// a key reused for another order is a conflict, not that order's replay.
	Terms string
}

// Validate refuses a request that cannot be recorded faithfully. Everything
// about whether it may be paid is the engine's to decide and is recorded as a
// decision, including a zero or negative amount.
//
// Text that Postgres cannot store, invalid UTF-8 or a NUL character, is
// refused here too. Let through, it would fail the decision's insert on every
// retry, and the attempt would never be recorded at all.
func (r Request) Validate() error {
	if r.IdempotencyKey == "" {
		return apperr.Invalid("an idempotency key is required")
	}
	if r.Amount.Asset() == "" {
		return apperr.Invalid("the amount has no asset")
	}
	if d := r.Amount.Decimal(); !d.Equal(d.Round(maxAmountScale)) || d.Abs().GreaterThanOrEqual(maxAmount) {
		return apperr.Invalid("the amount has more than 18 decimal places or is too large")
	}
	switch r.Signals.URLSource {
	case "", authz.URLSourceTask, authz.URLSourceFetched, authz.URLSourceUnknown:
	default:
		return apperr.Invalid("url_source must be task, fetched or unknown")
	}

	for _, f := range []struct {
		name, value string
		max         int
	}{
		{"idempotency key", r.IdempotencyKey, maxKeyLength},
		{"category", r.Category, maxCategoryLength},
		{"payee", r.PayTo, maxPayToLength},
		{"resource URL", r.Signals.ResourceURL, maxResourceURLLength},
		{"purpose", r.Signals.Purpose, maxPurposeLength},
		{"task", r.Signals.Task, maxTaskLength},
		{"session id", r.Signals.SessionID, maxSessionIDLength},
	} {
		if !utf8.ValidString(f.value) || strings.ContainsRune(f.value, 0) {
			return apperr.Invalid("the " + f.name + " is not valid text")
		}
		if utf8.RuneCountInString(f.value) > f.max {
			return apperr.Invalid(fmt.Sprintf("the %s is longer than %d characters", f.name, f.max))
		}
	}
	return nil
}

// hash identifies the request's content, so a key reused with a different
// body is caught (Task 5.2).
//
// The fields are listed here rather than taken from the structs, and each is
// omitted when empty. A field added later is then empty in every request
// made before it existed, and a retry that straddles the deploy hashes the
// same as its original instead of failing as a conflict.
func (r Request) hash() []byte {
	canonical := struct {
		AgentID                    id.AgentID      `json:"agent_id,omitempty"`
		Amount                     string          `json:"amount,omitempty"`
		Asset                      currency.Asset  `json:"asset,omitempty"`
		Category                   string          `json:"category,omitempty"`
		PayTo                      string          `json:"pay_to,omitempty"`
		ResourceURL                string          `json:"resource_url,omitempty"`
		URLSource                  authz.URLSource `json:"url_source,omitempty"`
		Purpose                    string          `json:"purpose,omitempty"`
		Task                       string          `json:"task,omitempty"`
		SessionID                  string          `json:"session_id,omitempty"`
		SessionNovelCounterparties *int            `json:"session_novel_counterparties,omitempty"`
		Terms                      string          `json:"terms,omitempty"`
	}{
		AgentID:                    r.AgentID,
		Amount:                     r.Amount.String(),
		Asset:                      r.Amount.Asset(),
		Category:                   r.Category,
		PayTo:                      r.PayTo,
		ResourceURL:                r.Signals.ResourceURL,
		URLSource:                  r.Signals.URLSource,
		Purpose:                    r.Signals.Purpose,
		Task:                       r.Signals.Task,
		SessionID:                  r.Signals.SessionID,
		SessionNovelCounterparties: r.Signals.SessionNovelCounterparties,
		Terms:                      r.Terms,
	}
	// Marshalling a struct of strings and an *int cannot fail.
	raw, _ := json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	return sum[:]
}

func (r Request) authz() authz.Request {
	return authz.Request{Amount: r.Amount, Category: r.Category, PayTo: r.PayTo, Signals: r.Signals}
}

// Status is where a payment stands. A decline is final at once; an
// authorization waits for the chain (Task 7.7).
type Status string

const (
	// StatusDeclined is a payment the engine refused. Nothing moved.
	StatusDeclined Status = "declined"
	// StatusAuthorized is a payment approved and debited, not yet settled.
	StatusAuthorized Status = "authorized"
	// StatusSettled is a payment whose transfer landed on-chain (Task 7.7).
	StatusSettled Status = "settled"
	// StatusExpired is a payment whose transfer can never land. Its funds and
	// budget were given back.
	StatusExpired Status = "expired"
	// StatusFailed is a payment whose transfer landed and failed. Its funds and
	// budget were given back.
	StatusFailed Status = "failed"
)

// PayRequest is one payment an agent asks for, as the merchant priced it.
type PayRequest struct {
	AccountID      id.AccountID
	AgentID        id.AgentID
	IdempotencyKey string

	// Requirement is the merchant's PAYMENT-REQUIRED, as the agent received it.
	Requirement []byte
	Category    string
	Signals     authz.Signals
}

// Paid is what Pay answers: the decision, and what to pay with if it was yes.
type Paid struct {
	Result

	// PaymentSignature is the PAYMENT-SIGNATURE header value. Empty on a
	// decline, and once the payment has expired or failed, when it is worthless.
	PaymentSignature string
}

// Recorded is a payment as it was stored: what the agent asked for, and where
// the payment stands now.
type Recorded struct {
	ID          id.PaymentID
	Amount      currency.Amount
	Category    string
	PayTo       string
	ResourceURL string

	// Status is current, not as decided: an authorization later settles,
	// expires or fails (Task 7.7).
	Status Status
}

// DeclineTotal is how many of one agent's payments in one asset were
// declined, and how much they asked for.
type DeclineTotal struct {
	AgentID id.AgentID
	Count   int

	// Amount sums the declines that asked for a positive amount. An
	// invalid_amount decline asked for nothing that could have moved, so it
	// is counted but adds nothing here.
	Amount currency.Amount
}

// Rail says how payments move. Recorded on every payment for reporting and
// reconciliation; no rule reads it (Task 5.1).
type Rail struct {
	// Name is the rail and scheme, for example "x402-exact-solana".
	Name string
	// Token is what moves on-chain. Always USDC today (§8.3).
	Token string
	// Network is a CAIP-2 chain id, for example Solana devnet.
	Network string
}

// Result is what Authorize answers, and what an idempotency key remembers.
type Result struct {
	PaymentID  id.PaymentID
	Status     Status
	Outcome    authz.Outcome
	ReasonCode authz.ReasonCode
	Reason     string
	DecidedAt  time.Time

	// Replayed is true when this is the remembered answer to an earlier
	// request with the same key, so the caller knows nothing new happened.
	Replayed bool
}

// storedResult is a Result as idempotency_key.response holds it. It is read
// back for a day, across deploys, so its shape changes only with v.
type storedResult struct {
	V          int              `json:"v"`
	PaymentID  id.PaymentID     `json:"payment_id"`
	Status     Status           `json:"status"`
	Outcome    authz.Outcome    `json:"outcome"`
	ReasonCode authz.ReasonCode `json:"reason_code"`
	Reason     string           `json:"reason"`
	DecidedAt  time.Time        `json:"decided_at"`
}

const storedResultVersion = 1

func encodeResult(r Result) ([]byte, error) {
	return json.Marshal(storedResult{
		V: storedResultVersion, PaymentID: r.PaymentID, Status: r.Status, Outcome: r.Outcome,
		ReasonCode: r.ReasonCode, Reason: r.Reason, DecidedAt: r.DecidedAt,
	})
}

func decodeResult(raw []byte) (Result, error) {
	var s storedResult
	if err := json.Unmarshal(raw, &s); err != nil {
		return Result{}, apperr.Internal("payment: remembered result is unreadable", err)
	}
	if s.V != storedResultVersion {
		return Result{}, apperr.Internal("payment: remembered result has an unknown version", nil)
	}
	return Result{
		PaymentID: s.PaymentID, Status: s.Status, Outcome: s.Outcome,
		ReasonCode: s.ReasonCode, Reason: s.Reason, DecidedAt: s.DecidedAt,
	}, nil
}

// errNoLongerAuthorized is a payment released by the settlement sweep while its
// transaction was being signed. The transaction is thrown away unseen.
var errNoLongerAuthorized = errors.New("payment: no longer authorized")

var (
	// ErrKeyReused is a key sent again with a different request. That is a
	// client bug, and answering with the first result would hide it (Task 5.2).
	ErrKeyReused = apperr.Conflict("this idempotency key was already used for a different request")

	// ErrAgentRevoked is an agent retired after it authenticated. It is
	// refused rather than recorded: the attempt carries no authority, and the
	// revocation is already the console's record of why.
	ErrAgentRevoked = apperr.Forbidden("this agent has been revoked")

	// ErrBusy is an authorization that lost a race for the same funds or
	// budget on every attempt. Nothing was charged or recorded.
	ErrBusy = apperr.RateLimited("too many payments are competing for this account, retry shortly").
		WithRetryAfter(time.Second)
)

// ErrTransactionInFlight is a payment whose transaction came out identical to
// another payment's: the same order, merchant memo included, signed on the
// same blockhash. The chain would take it once and both would settle from it,
// so this one is not handed out. It stays approved and unsigned; the same key
// retried once the blockhash moves on signs a transaction of its own, and
// without a retry the sweep releases it.
var ErrTransactionInFlight = apperr.Conflict("another payment holds an identical transaction, retry shortly").
	WithCode("transaction_in_flight").WithRetryAfter(time.Second)
