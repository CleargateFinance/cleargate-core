package ledger

// The only file in this module that knows Gin exists.
//
// The ledger has no HTTP surface of its own. Balances and history reach the
// console through the read endpoints that arrive with the account and decision
// log modules, and nothing outside the platform ever posts to the book
// directly. This file stays as the seam for that, should it be needed.
