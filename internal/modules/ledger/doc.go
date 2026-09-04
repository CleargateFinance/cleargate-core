// Package ledger is the double-entry book.
//
// This is the component that cannot be got wrong, so it is deliberately small
// and heavily tested. Three invariants hold at all times:
//
//  1. Append-only. Corrections are new reversing entries, never an UPDATE or a
//     DELETE, so the book can always be replayed to any point in history.
//  2. Every journal entry's postings sum to exactly zero, enforced by the
//     database itself rather than only by this code.
//  3. Agents hold spend counters, not balances. Money is pooled at the account
//     and per-agent limits are ceilings on behaviour, not reservations of
//     funds.
//
// Note the naming trap: a ledger account is an accounting bucket, and is a
// different thing from a customer account. They are never the same table.
package ledger
