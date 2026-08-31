// Package id defines typed identifiers.
//
// Typed IDs make it a compile error to pass an AccountID where an AgentID is
// expected. With bare strings that mistake is a runtime bug in the money path.
package id

type AccountID string
type AgentID string
type AgentSessionID string
type UserID string
type PrincipalID string
type MandateID string
type PaymentID string
type DecisionID string
type CounterpartyID string

// Ledger identifiers. Note that LedgerAccountID is an accounting bucket and is
// NOT interchangeable with AccountID, which is a customer. Keeping them as
// distinct types makes confusing the two a compile error.
type LedgerAccountID string
type JournalEntryID string
type PostingID string
