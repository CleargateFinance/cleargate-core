// Package fixtures provides shared test helpers — such as spinning up a
// throwaway Postgres instance for integration tests.
package fixtures

import (
	"testing"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
)

// TestDB wraps a database.DB pointed at a throwaway Postgres container. It
// embeds *database.DB so callers can use db.Pool, db.UoW and db.Exec exactly
// as they would against the real thing.
type TestDB struct {
	*database.DB
}

// NewTestDB starts a real Postgres, applies ./migrations, and returns a handle.
// Container lifetime is bound to the test via t.Cleanup.
func NewTestDB(t *testing.T) *TestDB {
	t.Helper()
	// TODO: postgres.Run(ctx, "postgres:17-alpine", ...); migrate up; return handle.
	panic("implement")
}
