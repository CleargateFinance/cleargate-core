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
	Do(ctx context.Context, fn func(ctx context.Context) error) error
}

type uow struct{ pool *pgxpool.Pool }

func NewUnitOfWork(pool *pgxpool.Pool) UnitOfWork {
	return &uow{pool: pool}
}

func (u *uow) Do(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(ctxKey{}).(pgx.Tx); ok {
		return fn(ctx)
	}

	tx, err := u.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}

	defer func() {
		_ = tx.Rollback(ctx)
	}()

	if err := fn(context.WithValue(ctx, ctxKey{}, tx)); err != nil {
		return err
	}

	return tx.Commit(ctx)
}
