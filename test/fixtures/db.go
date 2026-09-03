// Package fixtures provides shared test helpers, such as spinning up
// throwaway Postgres and Redis instances for integration tests.
//
// Real containers are used rather than mocks because most of this system's
// correctness lives in database constructs: the ledger's zero-sum constraint,
// the partial unique index enforcing one live mandate, generated columns
// keeping signed and queryable data in sync. A mock would test our Go against
// an imaginary database and pass while production is broken.
package fixtures

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	// Blank imports register the drivers with golang-migrate. They are
	// imported for their side effects only, which is why they have no name.
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
)

// TestDB wraps a database.DB pointed at a throwaway Postgres container. It
// embeds *database.DB so callers can use db.Pool, db.UoW and db.Exec exactly
// as they would against the real thing.
type TestDB struct {
	*database.DB

	// DSN is the connection string for the container, for tests that need to
	// connect separately.
	DSN string
}

// NewTestDB starts a real Postgres, applies ./migrations, and returns a handle.
//
// The container's lifetime is bound to the test through t.Cleanup, so it is
// torn down automatically even when the test fails or panics. Each call gets
// its own container, so tests cannot leak state into one another.
func NewTestDB(t *testing.T) *TestDB {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("cleargate_test"),
		tcpostgres.WithUsername("test"),
		tcpostgres.WithPassword("test"),
		// Postgres starts, runs its init scripts, then restarts. Waiting for
		// the ready log line twice avoids connecting during that first,
		// short-lived startup and getting dropped.
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("fixtures: start postgres: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("fixtures: terminate postgres: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("fixtures: connection string: %v", err)
	}

	applyMigrations(t, dsn)

	db, err := database.New(ctx, database.Options{
		DSN:            dsn,
		MaxConns:       4,
		MinConns:       1,
		ConnectTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("fixtures: connect: %v", err)
	}
	t.Cleanup(db.Close)

	return &TestDB{DB: db, DSN: dsn}
}

// applyMigrations brings the throwaway database up to the current schema.
//
// Tests run against the same migrations production does, so a migration that
// would break production breaks the test suite first.
func applyMigrations(t *testing.T, dsn string) {
	t.Helper()

	m, err := migrate.New("file://"+migrationsDir(t), dsn)
	if err != nil {
		t.Fatalf("fixtures: open migrations: %v", err)
	}
	defer func() {
		// Closing failures are teardown noise, worth logging but not failing
		// a test over.
		if sourceErr, dbErr := m.Close(); sourceErr != nil || dbErr != nil {
			t.Logf("fixtures: close migrate: source=%v database=%v", sourceErr, dbErr)
		}
	}()

	// ErrNoChange means there was nothing to apply, which is a valid state,
	// not a failure.
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("fixtures: apply migrations: %v", err)
	}
}

// migrationsDir resolves the absolute path to the repository's migrations
// directory.
//
// runtime.Caller is used rather than a relative path because Go runs each
// package's tests with that package's directory as the working directory, so a
// path like "../../migrations" would resolve differently depending on which
// package is running.
func migrationsDir(t *testing.T) string {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("fixtures: cannot resolve caller path")
	}

	// thisFile is <repo>/test/fixtures/db.go, so the repository root is two
	// levels up.
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")
}
