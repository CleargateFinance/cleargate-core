// Command migrate applies database migrations from ./migrations.
//
// Migrations run as a deliberate step, never automatically on API boot. A
// rolling deploy starts several API instances at once, and if each ran
// migrations they would race on the same DDL with an arbitrary winner. Running
// them once, from here, before the new version starts, avoids that entirely.
//
// Usage:
//
//	migrate up               apply every pending migration
//	migrate down             roll back exactly one migration
//	migrate version          print the current schema version
//	migrate force <version>  clear a dirty state, see below
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/golang-migrate/migrate/v4"

	// Blank imports register the drivers with golang-migrate. They are
	// imported for their side effects only, which is why they have no name.
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	"github.com/CleargateFinance/cleargate-core/internal/infrastructure/config"
)

func main() {
	migrationsPath := flag.String("path", "migrations", "directory holding the migration files")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// golang-migrate takes a source URL and a database URL. The file:// source
	// reads the .sql files, the DSN points at the database that records which
	// of them have already run.
	m, err := migrate.New("file://"+*migrationsPath, cfg.Database.DSN)
	if err != nil {
		log.Fatalf("migrate: open: %v", err)
	}
	// Both returned errors are connection teardown problems, not migration
	// failures, so they are reported without changing the exit code.
	defer func() {
		sourceErr, dbErr := m.Close()
		if sourceErr != nil {
			log.Printf("migrate: close source: %v", sourceErr)
		}
		if dbErr != nil {
			log.Printf("migrate: close database: %v", dbErr)
		}
	}()

	switch args[0] {
	case "up":
		runUp(m)
	case "down":
		runDown(m)
	case "version":
		runVersion(m)
	case "force":
		runForce(m, args)
	default:
		usage()
		os.Exit(2)
	}
}

// runUp applies every migration that has not run yet.
func runUp(m *migrate.Migrate) {
	// ErrNoChange means the database is already current. That is a success for
	// a deploy step, not a failure, so it must not exit non-zero.
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		log.Fatalf("migrate: up: %v", err)
	}
	log.Println("migrations applied")
	printVersion(m)
}

// runDown rolls back exactly one migration.
//
// Steps(-1) is used rather than Down(), which would roll back every migration
// and drop the whole schema. One step at a time is the recoverable operation,
// a full teardown almost never is.
func runDown(m *migrate.Migrate) {
	if err := m.Steps(-1); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		log.Fatalf("migrate: down: %v", err)
	}
	log.Println("rolled back one migration")
	printVersion(m)
}

// runVersion prints the current schema version.
func runVersion(m *migrate.Migrate) {
	printVersion(m)
}

// runForce clears a dirty state by asserting which version the schema is at.
//
// golang-migrate marks the schema dirty when a migration fails partway, and
// refuses to continue until a human confirms the real state. This is the
// escape hatch, and it is deliberately manual, since only a person can inspect
// the database and decide what actually got applied.
func runForce(m *migrate.Migrate, args []string) {
	if len(args) < 2 {
		log.Fatal("migrate: force requires a version, for example: migrate force 1")
	}

	version, err := strconv.Atoi(args[1])
	if err != nil {
		log.Fatalf("migrate: force: invalid version %q: %v", args[1], err)
	}

	if err := m.Force(version); err != nil {
		log.Fatalf("migrate: force: %v", err)
	}
	log.Printf("forced schema version to %d", version)
}

// printVersion reports the current version and whether the schema is dirty.
func printVersion(m *migrate.Migrate) {
	version, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		log.Println("schema version: none, no migrations have been applied")
		return
	}
	if err != nil {
		log.Fatalf("migrate: version: %v", err)
	}

	// Dirty means a migration failed partway and the schema is in an unknown
	// state. It must be resolved by hand before anything else runs.
	if dirty {
		log.Printf("schema version: %d (DIRTY, resolve with: migrate force <version>)", version)
		return
	}
	log.Printf("schema version: %d", version)
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: migrate [-path dir] <command>

Commands:
  up               apply every pending migration
  down             roll back exactly one migration
  version          print the current schema version
  force <version>  clear a dirty state by asserting the current version

The database connection is read from DATABASE_DSN.
`)
}
