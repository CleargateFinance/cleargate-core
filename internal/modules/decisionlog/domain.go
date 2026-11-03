package decisionlog

import (
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
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
