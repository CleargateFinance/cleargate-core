// Package payment orchestrates a single payment. It is the use case that binds
// the other modules together and owns the transaction boundary.
//
// Flow (whitepaper §5.4): resolve mandate -> authz.Decide (pure) -> open one
// database transaction -> debit ledger + increment spend counter + write
// decision record + append outbox event -> commit.
//
// Everything after Decide() must be atomic. A crash between "money moved" and
// "decision recorded" would leave a payment with no evidence behind it, which
// is precisely the state the guarantee cannot survive.
package payment
