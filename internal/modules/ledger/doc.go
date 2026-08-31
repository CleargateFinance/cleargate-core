// Package ledger is the double-entry book. Whitepaper §8.3, §9.3.
//
// This is the component that cannot be got wrong, so it is the smallest and
// most heavily tested. Three invariants:
//
//  1. Append-only. Corrections are reversing entries, never UPDATE or DELETE.
//  2. Every journal entry's postings sum to zero, enforced in the database.
//  3. Agents hold spend COUNTERS, not balances. Money is pooled at the account
//     (the corporate-card model); per-agent limits are ceilings on behaviour,
//     not reservations of funds.
//
// Note the naming trap: ledger_account (an accounting bucket) is a different
// thing from account (a customer). They are never the same table.
package ledger
