// Package mandate owns signed grants of spending authority.
//
// Whitepaper §4.1, §6.1. Two properties are non-negotiable:
//
//  1. Immutable and versioned. A mandate is never UPDATEd; changing rules
//     inserts a new version. Six months after a payment, a dispute must be able
//     to ask "what authority was in force at that instant" and get an answer.
//  2. Signed by the owner, verified server-side. A compromise of Cleargate's
//     own database therefore cannot fabricate spending authority.
//
// The MVP delivers the first in full and the second not yet. A mandate is
// approved by a console session with a recent TOTP step-up and stored as the
// exact bytes approved, so the passkey signature of Task 3.1 can later cover
// those same bytes. Until then a write that bypasses this module is not
// detectable from the row alone.
//
// MVP scope: caps (per tx / day / month) in one asset, categories, an optional
// payee allowlist, expiry, versioning.
package mandate
