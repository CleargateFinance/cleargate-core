// Package database owns the connection pool and transaction plumbing (backed
// by Postgres). It contains no business logic and no table-specific SQL —
// that lives in each module's repo_postgres.go file.
package database

// DB is the handle modules receive. It abstracts *pgxpool.Pool so a repository
// method works identically inside or outside an open transaction.
type DB interface{}

// TODO(scaffold): New(cfg) (*Pool, error), health check, pool tuning.
