package database

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UnitOfWork runs fn inside a single database transaction.
//
// This exists because authorize -> debit ledger -> record decision must be
// atomic. Any repository called with the returned context joins the same
// transaction, so modules compose transactionally without knowing about each
// other's storage.
type UnitOfWork interface {
	// Do method takes a `fn` param containing any kind of database work needs to happen atomically.
	// This is a function the rest of the app is allowed to call, without knowing how it works exactly with the DB.
	Do(ctx context.Context, fn func(ctx context.Context) error) error
}

// `pgxpool.Pool` is a manager for a batch of already-open network connections to Postgres.
// Opening a fresh TCP connection to a database is slow, especially on every single query.
// Instead the app opens several connections once at startup and keeps them alive.
// The pool hands one out whenever code needs to talk to the database, and takes it back when done.
// P.s: every Exec/Query/QueryRow call in `postgres.go` that hits `d.Pool.Exec()` is borrowing one connection from that stash,
// running one query on it, and returning it.
type uow struct{ pool *pgxpool.Pool }

// NewUnitOfWork builds a UnitOfWork backed by pool.
func NewUnitOfWork(pool *pgxpool.Pool) UnitOfWork {
	return &uow{pool: pool}
}

func (u *uow) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	// 1. Check whether there is an open tx already (to omit nested txs)
	if _, ok := ctx.Value(ctxKey{}).(pgx.Tx); ok {
		return fn(ctx) // if yes run fn inside existing tx
	}

	// 2. Borrow a connection from the pool, put it in draft mode
	tx, err := u.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	// 3. If anything failed before reaching `Commit()`, that deferred `Rollback()` discards the changes
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	// 4. - Stash the transaction into the context
	// 	  - After that run `fn` with that new context
	// 	  - If `fn` returned an error --> skip Commit, fall through, Do returns that error, and the deferred Rollback throws away everything `fn` did.
	if err := fn(context.WithValue(ctx, ctxKey{}, tx)); err != nil {
		return err
	}
	// - If `fn` succeeded `tx.Commit()` makes everything permanent
	return tx.Commit(ctx)
}
