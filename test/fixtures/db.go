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
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	// Blank imports register the drivers with golang-migrate. They are
	// imported for their side effects only, which is why they have no name.
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/database"
)

// TestDB wraps a database.DB pointed at a throwaway database. It embeds
// *database.DB so callers can use db.Pool, db.UoW and db.Exec exactly as they
// would against the real thing.
type TestDB struct {
	*database.DB

	// DSN is the connection string for the database, for tests that need to
	// connect separately.
	DSN string
}

// sharedPostgres is the one container a test process starts. Starting a
// container costs seconds and a package like account asks for twenty
// databases, so each test gets a database cloned from a migrated template
// instead of a container of its own. A clone is a file copy that takes tens of
// milliseconds, and tests still cannot leak state into one another because
// no two share a database.
var sharedPostgres struct {
	once sync.Once
	err  error

	// dsn names the template database; clones swap the database name in it.
	dsn  string
	next atomic.Int64
}

const templateDB = "cleargate_template"

// NewTestDB returns a handle to a fresh database at the current schema.
//
// The database is dropped when the test ends, including a failed or panicking
// one. The container behind it lives until the test process exits, where
// testcontainers' reaper removes it.
func NewTestDB(t *testing.T) *TestDB {
	t.Helper()
	ctx := context.Background()

	requireSharedPostgres(t)

	name := fmt.Sprintf("t%d", sharedPostgres.next.Add(1))
	exec(ctx, t, sharedPostgres.dsn, fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s`, name, templateDB))
	// Registered before db.Close below, so it runs after it: Cleanup is
	// last-in first-out, and a database cannot be dropped while connected.
	t.Cleanup(func() {
		exec(context.Background(), t, sharedPostgres.dsn, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
	})

	dsn := withDatabase(t, sharedPostgres.dsn, name)
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

// requireSharedPostgres starts the shared container on first use and fails the
// test if it cannot be started.
func requireSharedPostgres(t *testing.T) {
	t.Helper()
	sharedPostgres.once.Do(func() { sharedPostgres.err = startSharedPostgres(context.Background(), t) })
	if sharedPostgres.err != nil {
		t.Fatalf("fixtures: shared postgres: %v", sharedPostgres.err)
	}
}

// startSharedPostgres starts the container and migrates the template.
func startSharedPostgres(ctx context.Context, t *testing.T) error {
	container, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase(templateDB),
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
		return fmt.Errorf("start: %w", err)
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return fmt.Errorf("connection string: %w", err)
	}
	applyMigrations(t, dsn)
	sharedPostgres.dsn = dsn
	return nil
}

// withDatabase returns dsn pointed at another database on the same server.
func withDatabase(t *testing.T, dsn, name string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("fixtures: parse dsn: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// exec runs one statement outside any test database. CREATE and DROP DATABASE
// cannot run inside a transaction, so they get a plain connection.
func exec(ctx context.Context, t *testing.T, dsn, statement string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("fixtures: connect for %q: %v", statement, err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, statement); err != nil {
		t.Fatalf("fixtures: %s: %v", statement, err)
	}
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
