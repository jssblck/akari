// Package store is the akari-server data layer: a Postgres connection pool, the
// startup migration runner, and the query methods the rest of the server uses.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps a Postgres connection pool.
type Store struct {
	Pool *pgxpool.Pool
}

// Open connects to Postgres and verifies the connection.
func Open(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &Store{Pool: pool}, nil
}

// Close releases the connection pool.
func (s *Store) Close() { s.Pool.Close() }

// EnsureDatabase creates the database named in databaseURL when it does not yet
// exist, connecting to the server's "postgres" maintenance database to do so. It
// is a no-op when the database is already present.
//
// This supports local and worktree development (and CI), where the integration
// test database lives in a disposable Postgres that nothing else provisions: a
// fresh `eph up` brings up an empty server, so the tests create their own target
// rather than depending on an out-of-band seeding step. It is deliberately not
// called by the server itself, which never creates its own database.
func EnsureDatabase(ctx context.Context, databaseURL string) error {
	u, err := url.Parse(databaseURL)
	if err != nil {
		return fmt.Errorf("parse database url: %w", err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" {
		return fmt.Errorf("database url has no database name")
	}

	admin := *u
	admin.Path = "/postgres"
	conn, err := pgx.Connect(ctx, admin.String())
	if err != nil {
		return fmt.Errorf("connect maintenance database: %w", err)
	}
	defer conn.Close(ctx)

	var exists bool
	if err := conn.QueryRow(ctx,
		"SELECT EXISTS (SELECT FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		return fmt.Errorf("check database %q: %w", name, err)
	}
	if exists {
		return nil
	}

	// CREATE DATABASE cannot run inside a transaction or take a parameterized
	// name, so quote the identifier ourselves; the name comes from our own URL.
	quoted := `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		// Tolerate a concurrent creator winning the race (duplicate_database).
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "42P04" {
			return nil
		}
		return fmt.Errorf("create database %q: %w", name, err)
	}
	return nil
}

// Migrate applies every embedded migration not yet recorded, in lexical order,
// each inside its own transaction. It is safe to run on every startup.
func (s *Store) Migrate(ctx context.Context, migrationFS embed.FS) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration conn: %w", err)
	}
	defer conn.Release()
	pg := conn.Conn().PgConn()

	// Track applied migrations. The simple protocol lets us run multi-statement
	// scripts (transaction control, several DDL statements) in one round trip,
	// which pgx's default extended protocol forbids.
	if _, err := pg.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
		   version TEXT PRIMARY KEY,
		   applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		 )`).ReadAll(); err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}

	applied, err := s.appliedVersions(ctx)
	if err != nil {
		return err
	}

	names, err := migrationNames(migrationFS)
	if err != nil {
		return err
	}

	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		if applied[version] {
			continue
		}
		body, err := migrationFS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		script := "BEGIN;\n" + string(body) +
			"\n;\nINSERT INTO schema_migrations (version) VALUES (" + quote(version) + ");\nCOMMIT;"
		if _, err := pg.Exec(ctx, script).ReadAll(); err != nil {
			// The simple-protocol transaction is aborted on error; roll back so
			// the pooled connection is reusable.
			_, _ = pg.Exec(ctx, "ROLLBACK").ReadAll()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) appliedVersions(ctx context.Context) (map[string]bool, error) {
	rows, err := s.Pool.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("read applied migrations: %w", err)
	}
	defer rows.Close()
	applied := map[string]bool{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		applied[v] = true
	}
	return applied, rows.Err()
}

func migrationNames(migrationFS embed.FS) ([]string, error) {
	var names []string
	err := fs.WalkDir(migrationFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".sql") {
			names = append(names, path)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(names)
	return names, nil
}

// quote renders a SQL string literal. Migration versions are derived from file
// names under our control, but quoting keeps the script construction honest.
func quote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
