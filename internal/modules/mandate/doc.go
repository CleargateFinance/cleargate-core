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
// MVP scope: caps (per tx / day / month), categories, TTL, signing, versioning.
package mandate
