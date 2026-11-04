package decisionlog

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Record is one decision as it is stored: what was decided about a payment,
// why, against which mandate, and every input the engine saw.
//
// It carries plain values rather than the engine's types. This module keeps
// evidence; it does not interpret it, so a change to how decisions are made
// never changes how they are kept.
type Record struct {
	PaymentID id.PaymentID
	AccountID id.AccountID
	AgentID   id.AgentID

	Outcome    string
	ReasonCode string
	Reason     string

	// Mandate is the authority evaluated, or nil when the agent had none.
	Mandate *MandateRef

	// Features is the engine's snapshot of its inputs, already encoded as
	// JSON (Task 4.2). Stored as given and never re-encoded, so the record
	// holds exactly what the engine produced.
	Features []byte

	// Checks are the stages that ran, in order.
	Checks []Check

	// EvaluatedAt is the decision instant. It must equal the payment's
	// decided_at; the database refuses a decision that disagrees.
	EvaluatedAt time.Time
	Latency     time.Duration
}

// MandateRef names the exact mandate a decision was checked against: its
// version and the digest of the bytes approved, not just its row.
type MandateRef struct {
	ID      id.MandateID
	Version int
	Digest  []byte
}

// Check is one stage's result.
type Check struct {
	Stage  string
	Result string
}

// Validate checks what can be known without the database, so a malformed
// record fails with a message naming the problem rather than a constraint.
func (r Record) Validate() error {
	switch {
	case r.PaymentID == "" || r.AccountID == "" || r.AgentID == "":
		return apperr.Internal("decisionlog: record must name its payment, account and agent", nil)
	case r.Outcome == "" || r.ReasonCode == "":
		return apperr.Internal("decisionlog: record has no outcome or reason code", nil)
	case len(r.Features) == 0:
		return apperr.Internal("decisionlog: record has no feature snapshot", nil)
	case len(r.Checks) == 0:
		return apperr.Internal("decisionlog: record has no checks; every decision runs at least one stage", nil)
	case r.EvaluatedAt.IsZero():
		return apperr.Internal("decisionlog: record has no decision instant", nil)
	case r.Latency < 0:
		return apperr.Internal("decisionlog: negative latency", nil)
	}
	return nil
}

// Entry is one decision as the console's log shows it: what was decided and
// why, against which mandate, and what the payment asked for.
//
// It leaves out the feature snapshot. That is evidence kept in the engine's
// own versioned shape (Task 4.2), and it holds the agent's free text, which
// Task 5.4 moves out before anything is built on it; publishing it now would
// make both a public contract.
type Entry struct {
	ID         id.DecisionID
	PaymentID  id.PaymentID
	Outcome    string
	ReasonCode string
	Reason     string
	Mandate    *MandateRef
	Checks     []Check

	EvaluatedAt time.Time
	Latency     time.Duration

	Payment Payment
}

// Payment is what a decision was about, as the payment module recorded it.
type Payment struct {
	Amount      currency.Amount
	Category    string
	PayTo       string
	ResourceURL string

	// Status is where the payment stands now, which for an approval can have
	// moved on since the decision (Task 7.7).
	Status string
}

// Actor is the person reading the log, as their console session describes
// them.
type Actor struct {
	UserID id.UserID
}

// Page sizes. A page is one query and one lookup of its payments, and the
// largest is small enough that neither is slow.
const (
	DefaultPageSize = 50
	MaxPageSize     = 100
)

// Outcomes a query may filter on, the two the decision table holds.
const (
	OutcomeApproved = "approved"
	OutcomeDeclined = "declined"
)

// Query selects one page of an agent's log, newest first.
type Query struct {
	// Outcome keeps only approvals or only declines. Empty keeps both.
	Outcome string

	// Limit is the page size, DefaultPageSize when zero.
	Limit int

	// After continues from the last entry of an earlier page, or starts at
	// the newest when nil.
	After *Cursor
}

// Validate checks a query before it reaches the database, so a bad parameter
// is the caller's error rather than a failed statement.
func (q Query) Validate() error {
	switch {
	case q.Outcome != "" && q.Outcome != OutcomeApproved && q.Outcome != OutcomeDeclined:
		return apperr.Invalid("outcome must be approved or declined")
	case q.Limit < 0 || q.Limit > MaxPageSize:
		return apperr.Invalid(fmt.Sprintf("limit must be between 1 and %d", MaxPageSize))
	}
	return nil
}

// Cursor is a position in an agent's log: the last entry a page returned.
//
// It names the entry by its instant and id together. The instant alone is
// not unique, since two payments by one agent can be decided in the same
// microsecond, and a page boundary between them would skip or repeat one.
type Cursor struct {
	EvaluatedAt time.Time
	ID          id.DecisionID
}

// ErrInvalidCursor reports a cursor this API did not issue.
var ErrInvalidCursor = apperr.Invalid("cursor is not one this API returned")

// String encodes the cursor for the wire. It is opaque to clients, so the
// encoding can change without an API change. Microseconds, because that is
// what Postgres stores: a finer instant would never match the row it names.
func (c Cursor) String() string {
	raw := strconv.FormatInt(c.EvaluatedAt.UnixMicro(), 10) + "_" + string(c.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// ParseCursor decodes a cursor String produced.
func ParseCursor(s string) (Cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	micros, decisionID, ok := strings.Cut(string(raw), "_")
	if !ok {
		return Cursor{}, ErrInvalidCursor
	}
	n, err := strconv.ParseInt(micros, 10, 64)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	parsed, err := uuid.Parse(decisionID)
	if err != nil {
		return Cursor{}, ErrInvalidCursor
	}
	return Cursor{EvaluatedAt: time.UnixMicro(n).UTC(), ID: id.DecisionID(parsed.String())}, nil
}

// Page is one page of the log, and where the next one starts, or nil when
// this is the last.
type Page struct {
	Entries []Entry
	Next    *Cursor
}
