package db

import (
	"context"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/bhavyamsharmaa/recovery-agent/migrations"
)

// TestMigrationsAreEmbedded guards the one failure this package can have that
// still compiles and still starts: go:embed silently matching nothing, which
// would produce a server that boots happily against an empty schema.
func TestMigrationsAreEmbedded(t *testing.T) {
	versions, err := availableVersions()
	if err != nil {
		t.Fatalf("availableVersions: %v", err)
	}
	if len(versions) == 0 {
		t.Fatal("no migrations embedded")
	}
	if versions[0] != "001_init.sql" {
		t.Errorf("first migration = %q, want 001_init.sql", versions[0])
	}

	body, err := migrations.FS.ReadFile("001_init.sql")
	if err != nil {
		t.Fatalf("read 001_init.sql: %v", err)
	}
	for _, table := range []string{"schema_migrations", "failed_payments", "decisions", "outcomes"} {
		if !strings.Contains(string(body), "CREATE TABLE IF NOT EXISTS "+table) {
			t.Errorf("001_init.sql does not create %s", table)
		}
	}

	// Every statement is IF NOT EXISTS, which is what lets a database migrated
	// before this runner existed converge instead of erroring.
	for _, stmt := range []string{"CREATE TABLE ", "CREATE INDEX "} {
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, stmt) && !strings.Contains(line, "IF NOT EXISTS") {
				t.Errorf("statement is not idempotent: %q", line)
			}
		}
	}
}

// TestMigrationsAreLexicallyOrdered pins the naming convention the runner
// depends on: it applies files in sorted order, so a migration named without a
// zero-padded prefix would run at the wrong time.
func TestMigrationsAreLexicallyOrdered(t *testing.T) {
	entries, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	sorted := append([]string(nil), entries...)
	sort.Strings(sorted)

	for i, e := range entries {
		if e != sorted[i] {
			t.Fatalf("embedded order %v is not sorted order %v", entries, sorted)
		}
		if len(e) < 4 || !strings.HasPrefix(e[:3], "0") && e[0] < '0' || e[0] > '9' {
			t.Errorf("migration %q does not start with a numeric prefix", e)
		}
	}
}

// TestConnectRequiresDatabaseURL is the fail-fast contract: a missing variable
// is an error the caller can report, not a panic.
func TestConnectRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	pool, err := Connect(context.Background())
	if err == nil {
		pool.Close()
		t.Fatal("Connect succeeded with no DATABASE_URL")
	}
	if !strings.Contains(err.Error(), "DATABASE_URL is not set") {
		t.Errorf("error = %q, want it to name the missing variable", err)
	}
}

// TestMigrateIsIdempotent runs the real thing twice against the real database.
// Idempotency is the property that cannot be tested without one: it is about
// what the database already contains, not about what the code does.
//
// Skipped unless RECOVERY_LIVE_TESTS=1, matching the calibration test in
// internal/ingest.
func TestMigrateIsIdempotent(t *testing.T) {
	if os.Getenv("RECOVERY_LIVE_TESTS") != "1" {
		t.Skip("set RECOVERY_LIVE_TESTS=1 to run against the real database")
	}

	ctx := context.Background()
	pool, err := Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer pool.Close()

	// First call may or may not apply anything, depending on whether this
	// database has been migrated before. Either is correct.
	first, err := Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	t.Logf("first run applied: %v", first)

	// The second is the assertion: whatever state the first left, the second
	// must find nothing to do and must not error.
	second, err := Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("second Migrate errored, so it is not idempotent: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second Migrate applied %v, want nothing", second)
	}

	// A third, because idempotency that only holds for one repeat is a
	// coincidence.
	third, err := Migrate(ctx, pool)
	if err != nil {
		t.Fatalf("third Migrate: %v", err)
	}
	if len(third) != 0 {
		t.Errorf("third Migrate applied %v, want nothing", third)
	}

	for _, table := range []string{"schema_migrations", "failed_payments", "decisions", "outcomes"} {
		var exists bool
		if err := pool.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname='public' AND tablename=$1)`,
			table).Scan(&exists); err != nil {
			t.Fatalf("check %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s does not exist after migrating", table)
		}
	}

	// Recorded exactly once, not once per run.
	var n int
	if err := pool.QueryRowContext(ctx,
		`SELECT count(*) FROM schema_migrations WHERE version='001_init.sql'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("001_init.sql recorded %d times, want exactly 1", n)
	}
}
