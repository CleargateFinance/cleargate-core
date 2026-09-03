// Package database owns the connection pool and transaction plumbing (backed
// by Postgres). It contains no business logic and no table-specific SQL,
// those live in each module's repo_postgres.go file. The methods below are the
// single door every repo_postgres.go file uses to talk to the database.
package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ctxKey is the unique tag used to store and find the ambient transaction
// inside a context.Context. It is unexported so no other package can collide
// with it, or reach in and grab the transaction directly.
type ctxKey struct{}

// Options configures the pool. It is plain data rather than a config.Database,
// so this package does not depend on the config package. internal/app does the
// translation, being the only place that knows about both.
type Options struct {
	DSN            string
	MaxConns       int32
	MinConns       int32
	ConnectTimeout time.Duration
}

// DB is what repositories receive. Exec, Query and QueryRow transparently use
// the ambient transaction if one is open on the context, otherwise the pool.
type DB struct {
	Pool *pgxpool.Pool
	UoW  UnitOfWork
}

// New opens the connection pool and verifies it can actually reach Postgres.
//
// The verifying ping matters. Without it, pgxpool connects lazily, so a bad
// DSN or an unreachable database would boot "successfully" and only fail at
// the first real request, which is far harder to diagnose than a failed start.
func New(ctx context.Context, opts Options) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(opts.DSN)
	if err != nil {
		return nil, fmt.Errorf("database: parse dsn: %w", err)
	}

	poolCfg.MaxConns = opts.MaxConns
	poolCfg.MinConns = opts.MinConns

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("database: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, opts.ConnectTimeout)
	defer cancel()

	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: ping: %w", err)
	}

	return &DB{Pool: pool, UoW: NewUnitOfWork(pool)}, nil
}

// Ping reports whether the database is reachable. It backs the health endpoint.
func (d *DB) Ping(ctx context.Context) error {
	return d.Pool.Ping(ctx)
}

// Close releases every connection in the pool. Call it once, at shutdown.
func (d *DB) Close() {
	d.Pool.Close()
}

// q reports whether a transaction is open on ctx.
//
// A nil return means "no transaction, use the plain pool".
func (d *DB) q(ctx context.Context) pgx.Tx {
	// ctx.Value returns the generic type any, so the result is unwrapped back
	// to the concrete pgx.Tx type. The two-result form never panics, it just
	// reports ok as false when nothing was stashed.
	if tx, ok := ctx.Value(ctxKey{}).(pgx.Tx); ok {
		return tx // a transaction was stashed here, use it
	}
	return nil // nothing stashed, the caller should use the plain pool
}

// Exec joins the ambient transaction stashed in ctx by UnitOfWork.Do if one is
// open, otherwise it runs directly against the pool. This is what lets a
// repository call db.Exec(ctx, ...) without ever knowing whether it is inside
// a transaction.
func (d *DB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if tx := d.q(ctx); tx != nil {
		return tx.Exec(ctx, sql, args...) // run it as part of the open transaction
	}
	return d.Pool.Exec(ctx, sql, args...) // no transaction open, run it standalone
}

// Query joins the ambient transaction stashed in ctx by UnitOfWork.Do if one is
// open, otherwise it runs directly against the pool.
func (d *DB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if tx := d.q(ctx); tx != nil {
		return tx.Query(ctx, sql, args...)
	}
	return d.Pool.Query(ctx, sql, args...)
}

// QueryRow joins the ambient transaction stashed in ctx by UnitOfWork.Do if one
// is open, otherwise it runs directly against the pool.
func (d *DB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if tx := d.q(ctx); tx != nil {
		return tx.QueryRow(ctx, sql, args...)
	}
	return d.Pool.QueryRow(ctx, sql, args...)
}
