// Package authz is the authorization engine: the four checks of whitepaper §6.
//
// Design constraint that shapes everything here: Decide() performs NO I/O. It
// is a pure function of (request, mandate, precomputed signals) -> decision.
//
// Two reasons. First, the sub-100ms budget (§6.6) forbids chain calls, model
// inference or external HTTP in the request path. Second, this is the code an
// auditor and a disputing customer will read; a pure function is testable as a
// table of inputs and expected outcomes with no database.
//
// MVP scope: mandate check only. Counterparty, intent and behaviour checks are
// stubbed to Pass so the pipeline shape exists from day one.
package authz
