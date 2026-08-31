// Package account owns the identity half of the entity model: principal,
// account, user, account membership + role, agent, and agent credential.
//
// Whitepaper §4.1. The governing rule is "separate what something IS from what
// it DOES" — this package answers only what things are. Payer and payee are
// roles assumed per transaction and live in the payment module.
//
// MVP scope: one principal, one account, one implicit owner user, N agents.
package account
