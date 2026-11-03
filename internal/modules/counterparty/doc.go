// Package counterparty owns payees and their reputation. Whitepaper §4.1, §6.2.
//
// The subtle requirement: a counterparty record exists independently of any
// account and may later be LINKED to one when that party signs up, inheriting
// the reputation already accumulated. So counterparty is never a foreign key
// off account — account_id is a nullable claim on a counterparty.
//
// Identity is the endpoint the agent reports paying for, normalised to host
// and path, with the address the money went to as the fallback (Identify).
// It is immutable, and a counterparty is never deleted; the database refuses
// both.
//
// MVP scope: identity resolution and observation only. Scoring stays stubbed;
// the point at MVP is to start accumulating the record from transaction one.
package counterparty
