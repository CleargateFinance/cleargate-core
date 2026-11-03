package payment

// Interfaces this module needs from the outside world, declared by the
// consumer (this module) rather than the provider. Swapping an in-process
// implementation for a remote one later changes nothing in service.go.

import (
	"context"
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/outbox"
	"github.com/CleargateFinance/cleargate-core/internal/modules/authz"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// Repository is this module's own storage: payments and idempotency keys.
type Repository interface {
	// InsertPayment stores a payment and returns its id. The database refuses
	// to commit it without its decision, so call it inside the unit of work
	// that records one.
	InsertPayment(ctx context.Context, p Payment) (id.PaymentID, error)

	// Remembered returns the key's row if one was stored at or after since.
	Remembered(ctx context.Context, accountID id.AccountID, key string, since time.Time) (RememberedKey, bool, error)

	// Remember stores a final result under a key. A row for the same key
	// stored before expiredBefore is deleted first. A live row means a
	// concurrent request with this key committed first: Remember waits for
	// it, then reports errKeyTaken.
	Remember(ctx context.Context, k RememberedKey, expiredBefore time.Time) error

	// DeleteKeysBefore deletes keys stored before an instant, reporting how
	// many.
	DeleteKeysBefore(ctx context.Context, before time.Time) (int64, error)
}

// Payment is the row InsertPayment writes.
type Payment struct {
	AccountID   id.AccountID
	AgentID     id.AgentID
	Amount      currency.Amount
	Category    string
	PayTo       string
	ResourceURL string
	Rail        Rail
	Status      Status
	DecidedAt   time.Time

	// CounterpartyID is the payee, or nil when the payment named nothing to
	// identify one by.
	CounterpartyID *id.CounterpartyID
}

// RememberedKey is one idempotency_key row.
type RememberedKey struct {
	AccountID   id.AccountID
	Key         string
	RequestHash []byte
	PaymentID   id.PaymentID
	Response    []byte
	StoredAt    time.Time
}

// Accounts is what this module needs from the account module.
type Accounts interface {
	// Agent returns the agent, reporting the account module's not-found error
	// when there is none.
	Agent(ctx context.Context, agentID id.AgentID) (Agent, error)

	// SpendingBlocked reports whether the account is suspended or frozen.
	SpendingBlocked(ctx context.Context, accountID id.AccountID) (bool, error)

	// HoldSpending reports whether the account may still spend and the agent
	// is still active, and holds both until the transaction on ctx ends, so a
	// freeze or revocation cannot commit between this check and the payment.
	HoldSpending(ctx context.Context, accountID id.AccountID, agentID id.AgentID) (bool, error)
}

// Agent is what this module needs to know about the agent paying.
type Agent struct {
	ID        id.AgentID
	AccountID id.AccountID
	Active    bool
}

// Mandates is what this module needs from the mandate module.
type Mandates interface {
	// Current returns the agent's mandate in force now, or nil when it has
	// none. Having none is an input to the decision, not an error (Task 4.1).
	Current(ctx context.Context, agentID id.AgentID) (*Mandate, error)

	// HoldCurrent reports whether mandateID is still the one in force, and
	// holds it until the transaction on ctx ends, so a new version cannot
	// commit between this check and the payment.
	HoldCurrent(ctx context.Context, agentID id.AgentID, mandateID id.MandateID) (bool, error)
}

// Mandate is the authority a payment is checked against, with what the
// decision record must name about it.
type Mandate struct {
	ID      id.MandateID
	Version int
	Digest  []byte
	Rules   authz.Rules
}

// Counterparties is what this module needs from the counterparty module.
type Counterparties interface {
	// Resolve returns the payee's counterparty, creating it on the first
	// payment to it, or nil when there is nothing to identify it by.
	Resolve(ctx context.Context, p Payee) (*id.CounterpartyID, error)
}

// Payee is what a payment says about who it pays.
type Payee struct {
	ResourceURL string
	Network     string
	PayTo       string
}

// Ledger is what this module needs from the ledger.
type Ledger interface {
	// Balance returns the account's spendable funds.
	Balance(ctx context.Context, accountID id.AccountID, asset currency.Asset) (currency.Amount, error)

	// Spent returns the agent's spend in the UTC day and month containing at,
	// as zero of asset when there is none.
	Spent(ctx context.Context, agentID id.AgentID, asset currency.Asset, at time.Time) (day, month currency.Amount, err error)

	// Debit posts a payment: the amount leaves the account's funds for payee
	// payable, attributed to the agent and the payment. funded is false when
	// the funds were not there, and then nothing was written.
	Debit(ctx context.Context, d Debit) (funded bool, err error)

	// RecordSpend adds amount to the agent's day and month counters at at,
	// atomically, and returns the new totals.
	RecordSpend(ctx context.Context, agentID id.AgentID, amount currency.Amount, at time.Time) (day, month currency.Amount, err error)
}

// Debit is one payment's ledger posting.
type Debit struct {
	PaymentID id.PaymentID
	AccountID id.AccountID
	AgentID   id.AgentID
	Amount    currency.Amount
	At        time.Time
}

// DecisionLog is what this module needs from the decision log.
type DecisionLog interface {
	// Record stores a decision, joining the transaction open on ctx.
	Record(ctx context.Context, r DecisionRecord) error
}

// DecisionRecord is one decision as this module hands it over.
type DecisionRecord struct {
	PaymentID id.PaymentID
	AccountID id.AccountID
	AgentID   id.AgentID

	Decision authz.Decision

	// Features is Decision.Features encoded as JSON (Task 4.2).
	Features []byte

	// Mandate is the authority evaluated, or nil when the agent had none.
	Mandate *Mandate

	EvaluatedAt time.Time
	Latency     time.Duration
}

// Outbox appends events in the caller's transaction.
type Outbox interface {
	Append(ctx context.Context, e outbox.Event) error
}

// Clock supplies the current time, so the decision instant and key expiry
// are testable without waiting.
type Clock interface {
	Now() time.Time
}
