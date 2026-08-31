// Package postgres owns the database connection pool and transaction plumbing.
// It contains no business logic and no SQL statements — those live in each
// module's repo_postgres.go file.
package postgres

// DB is the handle modules receive. It abstracts *pgxpool.Pool so a repository
// method works identically inside or outside an open transaction.
type DB interface{}

// TODO(scaffold): New(cfg) (*Pool, error), health check, pool tuning.
