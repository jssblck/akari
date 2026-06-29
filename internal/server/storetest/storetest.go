// Package storetest provisions throwaway, fully isolated Postgres databases for
// the akari server's integration tests.
//
// NewStore is the one entry point every integration test uses, in the spirit of
// Rust's sqlx::test: it hands back a Store backed by a uniquely named database
// that was just created and migrated, and dropped again when the test finishes.
// Because no two tests share a database, the suite runs at Go's default package
// parallelism and individual tests may call t.Parallel. This replaces the older
// shared-database harness, where each test reset the global `public` schema in
// one database, so concurrent packages clobbered each other's schema_migrations
// table and the suite had to be serialized with `go test -p 1`.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/migrations"
)

// EnvDatabaseURL is the environment variable that opts a run in to the
// integration tests. Its value is a normal Postgres URL; only the host and
// credentials are used to reach the server, since each test's database is created
// beside the one the URL names rather than being it.
const EnvDatabaseURL = "AKARI_TEST_DATABASE_URL"

// NewStore returns a Store backed by its own isolated, freshly migrated database.
// The test is skipped when EnvDatabaseURL is unset, so a developer without a
// Postgres handy still gets a green (skipped) run.
//
// The database is dropped on t.Cleanup with WITH (FORCE), which terminates any
// connection still attached. That keeps cleanup robust when a test fails or
// leaves its pool open, so a run never leaks databases behind it.
func NewStore(t *testing.T) *store.Store {
	t.Helper()
	dbURL := provision(t)

	ctx := context.Background()
	st, err := store.Open(ctx, dbURL)
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx, migrations.FS); err != nil {
		t.Fatalf("migrate test store: %v", err)
	}
	return st
}

// provision creates an isolated, empty database and returns its connection
// string, registering the force-drop cleanup before it returns so a test that
// fails between here and NewStore's return still drops its database.
func provision(t *testing.T) string {
	t.Helper()
	base := os.Getenv(EnvDatabaseURL)
	if base == "" {
		t.Skipf("set %s to run integration tests", EnvDatabaseURL)
	}

	admin, err := maintenanceURL(base)
	if err != nil {
		t.Fatalf("derive maintenance url: %v", err)
	}
	name := uniqueDBName(t)

	ctx := context.Background()
	if err := createDatabase(ctx, admin, name); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		if err := dropDatabase(context.Background(), admin, name); err != nil {
			t.Errorf("drop test database %q: %v", name, err)
		}
	})

	return testDBURL(base, name)
}

// maintenanceURL rewrites a database URL to address the server's "postgres"
// maintenance database. CREATE DATABASE and DROP DATABASE cannot run while
// connected to their target, and the maintenance database always exists, so it is
// where that provisioning happens.
func maintenanceURL(databaseURL string) (string, error) {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return "", fmt.Errorf("parse database url: %w", err)
	}
	u.Path = "/postgres"
	return u.String(), nil
}

// testDBURL rewrites the base URL to address the per-test database and bounds its
// connection pool. The cap keeps a fully parallel suite (many pools at once) well
// under the server's default connection limit, since each test only needs a
// handful of connections.
func testDBURL(base, name string) string {
	u, _ := url.Parse(base) // already parsed and validated by maintenanceURL
	u.Path = "/" + name
	q := u.Query()
	q.Set("pool_max_conns", "4")
	u.RawQuery = q.Encode()
	return u.String()
}

// createDatabase creates a database by name over a one-shot maintenance
// connection. CREATE DATABASE cannot run inside a transaction or take a bind
// parameter, so the name (which this package generates) is quoted as an
// identifier rather than passed as an argument.
func createDatabase(ctx context.Context, adminURL, name string) error {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect maintenance database: %w", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+quoteIdent(name)); err != nil {
		return fmt.Errorf("create database %q: %w", name, err)
	}
	return nil
}

// dropDatabase force-drops a database by name. WITH (FORCE) (Postgres 13+)
// terminates any session still attached, so cleanup succeeds even when a failed
// test left its pool open; IF EXISTS makes a repeat drop a no-op.
func dropDatabase(ctx context.Context, adminURL, name string) error {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect maintenance database: %w", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(name)+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop database %q: %w", name, err)
	}
	return nil
}

// quoteIdent wraps a Postgres identifier in double quotes, doubling any embedded
// quote. The names here come from a fixed alphabet, but quoting keeps the
// statement construction honest.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// uniqueDBName builds a collision-free, lower-case database name that still
// carries the test's name, so a database that ever leaks is traceable to the test
// that made it. A random suffix guarantees uniqueness across parallel tests and
// repeat runs; the whole name is bounded to Postgres's 63-byte identifier limit.
func uniqueDBName(t *testing.T) string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("read random: %v", err)
	}
	return dbName(t.Name(), hex.EncodeToString(b[:]))
}

// dbName composes a bounded, lower-case Postgres identifier from a test name and
// a random suffix: a fixed prefix, the slugified name, then the suffix. The slug
// is truncated so the whole identifier stays within Postgres's 63-byte limit, and
// the suffix is always kept so uniqueness survives the truncation.
func dbName(testName, suffix string) string {
	const prefix = "akari_test_"
	slug := slugify(testName)
	if max := 63 - len(prefix) - 1 - len(suffix); len(slug) > max {
		slug = slug[:max]
	}
	return prefix + slug + "_" + suffix
}

// slugify reduces a test name to a lower-case token safe to embed in a Postgres
// identifier: anything outside [a-z0-9] becomes an underscore, and runs are
// trimmed at the ends so the slug reads cleanly.
func slugify(name string) string {
	var sb strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
		} else {
			sb.WriteByte('_')
		}
	}
	return strings.Trim(sb.String(), "_")
}
