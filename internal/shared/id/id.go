// Package id defines typed identifiers.
//
// Typed IDs make it a compile error to pass an AccountID where an AgentID is
// expected. With bare strings that mistake is a runtime bug in the money path.
package id

// AccountID identifies a customer account.
type AccountID string

// AgentID identifies an autonomous agent connected to an account.
type AgentID string

// AgentSessionID groups payments into one agent run.
type AgentSessionID string

// UserID identifies a human who logs into an account.
type UserID string

// UserSessionID identifies one human login session. It is distinct from
// AgentSessionID, which groups an agent's payments into a run: one is a
// credential's lifetime, the other is a unit of work.
type UserSessionID string

// PrincipalID identifies the human or legal entity ultimately responsible
// for one or more accounts.
type PrincipalID string

// MandateID identifies one signed, versioned grant of spending authority.
type MandateID string

// PaymentID identifies a single payment request, approved or declined.
type PaymentID string

// DecisionID identifies one evaluated-payment record in the decision log.
type DecisionID string

// CounterpartyID identifies a payee, on- or off-platform.
type CounterpartyID string

// LedgerAccountID identifies an accounting bucket. It is NOT interchangeable
// with AccountID, which is a customer. Keeping them as distinct types makes
// confusing the two a compile error.
type LedgerAccountID string

// JournalEntryID identifies one balanced double-entry journal entry.
type JournalEntryID string

// PostingID identifies a single posting (line) within a journal entry.
type PostingID string
