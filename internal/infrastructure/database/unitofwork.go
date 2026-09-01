package database

import "context"

// UnitOfWork runs fn inside a single database transaction.
//
// This exists because authorize -> debit ledger -> record decision must be
// atomic. Any repository called with the returned context joins the same
// transaction, so modules compose transactionally without knowing about each
// other's storage.
type UnitOfWork interface {
	Do(ctx context.Context, fn func(ctx context.Context) error) error
}
