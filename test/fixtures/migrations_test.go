//go:build integration

package fixtures

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// schemaQueries describe the catalog, one row per object. Each is ordered so
// the snapshot is deterministic. Column ordinal positions are left out on
// purpose: dropping and re-adding a column leaves a gap in them, which says
// nothing about whether the schema is the same.
var schemaQueries = []struct{ name, sql string }{
	{"extensions", `SELECT extname FROM pg_extension ORDER BY 1`},
	{"relations", `
		SELECT c.relname || ' ' || c.relkind::text
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND c.relname <> 'schema_migrations'
		ORDER BY 1`},
	{"columns", `
		SELECT table_name || '.' || column_name || ' ' || data_type || ' null=' || is_nullable
			|| ' default=' || coalesce(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name <> 'schema_migrations'
		ORDER BY 1`},
	{"constraints", `
		SELECT conrelid::regclass::text || ' ' || conname || ' ' || pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE connamespace = 'public'::regnamespace AND conrelid::regclass::text <> 'schema_migrations'
		ORDER BY 1`},
	{"indexes", `SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND tablename <> 'schema_migrations' ORDER BY 1`},
	{"triggers", `SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE NOT tgisinternal ORDER BY 1`},
	{"functions", `
		SELECT pg_get_functiondef(oid)
		FROM pg_proc WHERE pronamespace = 'public'::regnamespace
		ORDER BY 1`},
	{"types", `
		SELECT t.typname || ' ' || t.typtype::text || ' ' || coalesce(
			(SELECT string_agg(e.enumlabel, ',' ORDER BY e.enumsortorder) FROM pg_enum e WHERE e.enumtypid = t.oid), '')
		FROM pg_type t
		WHERE t.typnamespace = 'public'::regnamespace AND t.typtype IN ('e', 'd')
		ORDER BY 1`},
	{"views", `SELECT viewname || ' ' || definition FROM pg_views WHERE schemaname = 'public' ORDER BY 1`},
}

// schemaSnapshot renders everything the migrations create as one string, so
// two schemas compare with a single assertion and a readable diff.
func schemaSnapshot(ctx context.Context, t *testing.T, dsn string) string {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	defer func() { _ = conn.Close(ctx) }()

	var out strings.Builder
	for _, q := range schemaQueries {
		rows, err := conn.Query(ctx, q.sql)
		require.NoError(t, err, q.name)
		fmt.Fprintf(&out, "== %s\n", q.name)
		for rows.Next() {
			var line string
			require.NoError(t, rows.Scan(&line), q.name)
			out.WriteString(line + "\n")
		}
		require.NoError(t, rows.Err(), q.name)
		rows.Close()
	}
	return out.String()
}

// TestMigrations_RoundTrip applies the migrations one at a time, rolls them
// back one at a time, and applies them again.
//
// Every other integration test only ever runs the up files, so a down file
// that is wrong, or that leaves an object behind, would surface during an
// incident, the one moment `make migrate-down` is wanted. Comparing the schema
// after each step to the schema recorded on the way up proves each down file
// undoes exactly its own up file, and the final comparison proves a rolled
// back database can be migrated forward again.
func TestMigrations_RoundTrip(t *testing.T) {
	ctx := context.Background()
	requireSharedPostgres(t)

	// The shared template is already migrated, so this needs a database of
	// its own that starts empty.
	name := "migrations_round_trip"
	exec(ctx, t, sharedPostgres.dsn, fmt.Sprintf(`CREATE DATABASE %s TEMPLATE template0`, name))
	t.Cleanup(func() {
		exec(context.Background(), t, sharedPostgres.dsn, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
	})
	dsn := withDatabase(t, sharedPostgres.dsn, name)

	ups, err := filepath.Glob(filepath.Join(migrationsDir(t), "*.up.sql"))
	require.NoError(t, err)
	require.NotEmpty(t, ups, "no migrations found, the path is probably wrong")

	m, err := migrate.New("file://"+migrationsDir(t), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = m.Close() })

	// snapshots[i] is the schema with the first i migrations applied.
	snapshots := []string{schemaSnapshot(ctx, t, dsn)}
	for i := 1; i <= len(ups); i++ {
		require.NoError(t, m.Steps(1), "apply migration %d", i)
		snapshots = append(snapshots, schemaSnapshot(ctx, t, dsn))
	}

	for i := len(ups); i >= 1; i-- {
		require.NoError(t, m.Steps(-1), "roll back migration %d", i)
		assert.Equal(t, snapshots[i-1], schemaSnapshot(ctx, t, dsn),
			"rolling back migration %d did not restore the schema from before it", i)
	}

	require.NoError(t, m.Up(), "apply every migration again after a full rollback")
	assert.Equal(t, snapshots[len(ups)], schemaSnapshot(ctx, t, dsn),
		"the schema after rolling back and re-applying differs from the first run")
}
