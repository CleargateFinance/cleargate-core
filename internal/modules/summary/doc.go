// Package summary is the console's front page for one account: what it
// holds, what each agent has spent against its caps, and how many payments
// were declined. Whitepaper §12.1, §12.2.
//
// It owns no tables. Every figure is read from the module that keeps it,
// through the ports in ports.go, so a summary can never disagree with the
// book, the counters or the payments it reports on. It holds three rules:
//
//   - Any member may read it, a viewer included, and nobody else learns
//     anything from it (the account module decides).
//   - Spending is shown in the calendar windows the caps are enforced in,
//     UTC day and month (Task 1.3), so the burn-down a person reads is the
//     one a payment is checked against.
//   - The balance is shown at par. It is a claim on USDC, and until the peg
//     monitor exists (Phase 8) there is no market price to show beside it.
//     The API's "Known gaps" page says so (Task 6.2).
package summary
