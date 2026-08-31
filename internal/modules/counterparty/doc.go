// Package counterparty owns payees and their reputation. Whitepaper §4.1, §6.2.
//
// The subtle requirement: a counterparty record exists independently of any
// account and may later be LINKED to one when that party signs up, inheriting
// the reputation already accumulated. So counterparty is never a foreign key
// off account — account_id is a nullable claim on a counterparty.
//
// MVP scope: identity resolution and observation only. Scoring stays stubbed;
// the point at MVP is to start accumulating the record from transaction one.
package counterparty
