// Package store is the akari-server data layer: a Postgres connection pool, the
// startup migration runner, and the query methods the rest of the server uses.
package store

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

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
