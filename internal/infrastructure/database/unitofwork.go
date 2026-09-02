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

func NewUnitOfWork(pool *pgxpool.Pool) UnitOfWork {
	return &uow{pool: pool}
}

func (u *uow) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(ctxKey{}).(pgx.Tx); ok {
		return fn(ctx)
	}

	tx, err := u.pool.Begin(ctx) // borrow a connection FROM the pool, put it in draft mode
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	defer func() { // if anything failed before reaching `Commit()`, that deferred `Rollback()` discards the changes draft
		_ = tx.Rollback(ctx)
	}()

	if err := fn(context.WithValue(ctx, ctxKey{}, tx)); err != nil { // run the code — its queries go into the draft
		return err
	}

	return tx.Commit(ctx)
}
