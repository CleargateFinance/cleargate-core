// Package database owns the connection pool and transaction plumbing (backed
// by Postgres). It contains no business logic and no table-specific SQL —
// that lives in each module's repo_postgres.go file. Below methods are the single door
// every repo_postgres.go file uses to talk to the database
package database

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Unique tag used to store and find one specific value inside a `context.Context`
type ctxKey struct{}

// DB is what repositories receive. Exec/Query transparently use the ambient
// transaction if one is open on the context, otherwise the pool.
type DB struct {
	Pool *pgxpool.Pool
	UoW  UnitOfWork
}

// `q` method checks if there is an open transaction right now, or not
func (d *DB) q(ctx context.Context) pgx.Tx { // nil means "use pool"
	if tx, ok := ctx.Value(ctxKey{}).(pgx.Tx); ok { // `ctx.Value()` always returns the generic type `any`. Instead we unwrapping returned value from ctx as a concrete type `pgx.Tx`.
		return tx // yes — a transaction was stashed here, use it
	}
	return nil // no — nothing stashed, caller should use the plain pool
}

// Exec, Query and QueryRow join the ambient transaction stashed in ctx by
// UnitOfWork.Do, if one is open, otherwise they run directly against the pool.
// This is what lets a repository call db.Exec(ctx, ...) without ever knowing
// whether it's inside a transaction.
func (d *DB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if tx := d.q(ctx); tx != nil {
		return tx.Exec(ctx, sql, args...) // run it as part of the open transaction
	}
	return d.Pool.Exec(ctx, sql, args...) // no transaction open — run it standalone
}

func (d *DB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if tx := d.q(ctx); tx != nil {
		return tx.Query(ctx, sql, args...)
	}
	return d.Pool.Query(ctx, sql, args...)
}

func (d *DB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if tx := d.q(ctx); tx != nil {
		return tx.QueryRow(ctx, sql, args...)
	}
	return d.Pool.QueryRow(ctx, sql, args...)
}
