package ledger

import (
	"time"

	"github.com/CleargateFinance/cleargate-core/internal/shared/apperr"
	"github.com/CleargateFinance/cleargate-core/internal/shared/currency"
	"github.com/CleargateFinance/cleargate-core/internal/shared/id"
)

// AccountType names a kind of accounting bucket.
//
// A customer can own several buckets at once, because role and asset both
// split them. Spendable funds and money owed to that same party as a payee are
// economically different things, and two assets are never fungible with each
// other, so each combination is its own bucket.
type AccountType string

const (
	// TypeCustomerFunds holds a customer's spendable balance.
	TypeCustomerFunds AccountType = "customer_funds"
	// TypePayeePayable holds value owed to a payee but not yet settled out.
	TypePayeePayable AccountType = "payee_payable"
	// TypeRevenue holds platform earnings.
	TypeRevenue AccountType = "revenue"
	// TypeReserve holds capital held back against losses.
	TypeReserve AccountType = "reserve"
	// TypeOnchainFloat holds value actually sitting on a chain.
	TypeOnchainFloat AccountType = "onchain_float"
)

// Valid reports whether t is a recognised bucket type.
func (t AccountType) Valid() bool {
	switch t {
	case TypeCustomerFunds, TypePayeePayable, TypeRevenue, TypeReserve, TypeOnchainFloat:
		return true
	default:
		return false
	}
}

// EntryKind names what caused an entry to be written.
type EntryKind string

const (
	// KindPayment records an agent paying a payee.
	KindPayment EntryKind = "payment"
	// KindFunding records money arriving into an account from outside.
	KindFunding EntryKind = "funding"
	// KindPayout records money leaving to a payee's own destination.
	KindPayout EntryKind = "payout"
	// KindRefund records value returning to the payer.
	KindRefund EntryKind = "refund"
	// KindReversal undoes an earlier entry that should not have been written.
	KindReversal EntryKind = "reversal"
	// KindCorrection adjusts for an operational mistake.
	KindCorrection EntryKind = "correction"
)

// Valid reports whether k is a recognised entry kind.
func (k EntryKind) Valid() bool {
	switch k {
	case KindPayment, KindFunding, KindPayout, KindRefund, KindReversal, KindCorrection:
		return true
	default:
		return false
	}
}

// Line is one side of an entry: an amount moving into or out of one bucket.
//
// The amount is signed, negative to debit the bucket and positive to credit
// it, which is what makes the balance rule a plain sum rather than a
// conditional over a separate direction flag.
type Line struct {
	LedgerAccountID id.LedgerAccountID
	Amount          currency.Amount
}

// Entry is one economic event together with the lines that make it up.
type Entry struct {
	Kind  EntryKind
	Asset currency.Asset
	Lines []Line

	// Reference points at whatever caused this entry, for example a payment.
	// Both fields are optional, and are set together or not at all.
	ReferenceType string
	ReferenceID   *id.PaymentID

	// AgentID attributes the entry to the agent whose activity produced it,
	// when one did. This is what lets per-agent spending be recomputed from
	// the postings rather than trusted from the counters alone.
	AgentID *id.AgentID

	Description string

	// OccurredAt stamps both the entry and every posting under it. Leaving it
	// zero lets the store apply the current time.
	OccurredAt time.Time
}

// Debit builds a line that takes value out of a bucket.
func Debit(account id.LedgerAccountID, amount currency.Amount) Line {
	return Line{LedgerAccountID: account, Amount: amount.Neg()}
}

// Credit builds a line that puts value into a bucket.
func Credit(account id.LedgerAccountID, amount currency.Amount) Line {
	return Line{LedgerAccountID: account, Amount: amount}
}

// Transfer builds the two lines of a simple movement between buckets.
//
// Most entries are exactly this shape, and building them through one helper
// removes the chance of writing both lines with the same sign.
func Transfer(from, to id.LedgerAccountID, amount currency.Amount) []Line {
	return []Line{Debit(from, amount), Credit(to, amount)}
}

// Balanced reports whether lines form a valid double-entry set, meaning their
// signed amounts cancel out exactly.
//
// This is a pure function so the rule can be tested exhaustively without a
// database, and so it can be called cheaply before any write is attempted.
func Balanced(asset currency.Asset, lines []Line) (bool, error) {
	amounts := make([]currency.Amount, 0, len(lines))
	for _, l := range lines {
		amounts = append(amounts, l.Amount)
	}

	total, err := currency.Sum(asset, amounts...)
	if err != nil {
		return false, err
	}
	return total.IsZero(), nil
}

// Validate checks everything about an entry that can be known without touching
// the database.
//
// The database enforces the balance rule too, and that enforcement is what
// actually makes the invariant true. This check exists so the common case
// fails early with a message naming the real problem, rather than surfacing as
// a constraint violation at commit.
func (e Entry) Validate() error {
	if !e.Kind.Valid() {
		return apperr.Invalid("ledger: unknown entry kind " + string(e.Kind))
	}
	if e.Asset == "" {
		return apperr.Invalid("ledger: entry has no asset")
	}

	// Two lines is the minimum that can cancel out. One line can only be zero,
	// which is not a movement at all.
	if len(e.Lines) < 2 {
		return apperr.Invalid("ledger: entry needs at least two lines")
	}

	for _, l := range e.Lines {
		if l.LedgerAccountID == "" {
			return apperr.Invalid("ledger: line has no ledger account")
		}
		if l.Amount.IsZero() {
			return apperr.Invalid("ledger: line amount must not be zero")
		}
		if l.Amount.Asset() != e.Asset {
			return apperr.Invalid("ledger: line asset " + string(l.Amount.Asset()) +
				" does not match entry asset " + string(e.Asset))
		}
	}

	balanced, err := Balanced(e.Asset, e.Lines)
	if err != nil {
		return apperr.Invalid("ledger: " + err.Error())
	}
	if !balanced {
		return apperr.Invalid("ledger: entry does not sum to zero")
	}

	return nil
}
