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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps a Postgres connection pool.
type Store struct {
	Pool *pgxpool.Pool

	// parserEpoch is the running binary's parse.Epoch, injected at wiring time
	// (SetParserEpoch) because the store cannot import the parse package. It
	// drives the epoch-staleness gate on the aggregate OG-card snapshots: while
	// any session's projection is behind this epoch, a cross-session view could
	// mix old and new parses, so those snapshots decline to cache. Zero (never
	// set) gates nothing, which is what tests that never touch epochs want.
	parserEpoch int

	// windowSessionRowsReadHook is a deterministic concurrency seam used by the
	// snapshot regression test. Production stores leave it nil.
	windowSessionRowsReadHook func()

	// sweepBatchCommittedHook observes each sweep batch after its commit is
	// acknowledged, receiving the batch's removal count. It is the deterministic
	// seam for the sweep-cancellation test: polling the table for a batch's
	// effect races the server-side commit against the client-side
	// acknowledgment, so a cancel timed off the poll can still abort the Commit
	// call and drop a durable batch from the reported count. Production stores
	// leave it nil.
	sweepBatchCommittedHook func(batchRemoved int)
}

// SetParserEpoch records the running binary's parser epoch for the
// epoch-staleness gate (see epochGatedSnapshot). Call it once at wiring time,
// before serving traffic.
func (s *Store) SetParserEpoch(epoch int) { s.parserEpoch = epoch }

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
	lockAttempted := false
	defer func() {
		if !lockAttempted {
			conn.Release()
			return
		}
		// The caller's context may have expired because a migration was cancelled.
		// Use a short cleanup context so a session-level advisory lock never returns
		// to the pool with the connection that owns it. If unlock fails, discard the
		// connection; closing its backend releases the lock in PostgreSQL.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var unlocked bool
		if err := conn.QueryRow(cleanupCtx,
			`SELECT pg_advisory_unlock(
			   hashtext(current_database()),
			   hashtext('akari-schema-migrations')
			 )`).Scan(&unlocked); err != nil || !unlocked {
			raw := conn.Hijack()
			_ = raw.Close(context.Background())
			return
		}
		conn.Release()
	}()

	// Startup is multi-instance. Serialize the whole read-and-apply sequence on
	// this dedicated connection so two fresh replicas cannot both observe a
	// missing version and interleave its DDL. The database name scopes the lock
	// within a shared PostgreSQL cluster.
	//
	// Mark the attempt before sending it. Cancellation can race with PostgreSQL
	// granting the lock, so an error does not prove this session owns no lock.
	lockAttempted = true
	if _, err := conn.Exec(ctx,
		`SELECT pg_advisory_lock(
		   hashtext(current_database()),
		   hashtext('akari-schema-migrations')
		 )`); err != nil {
		return fmt.Errorf("lock schema migrations: %w", err)
	}
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

	applied, err := appliedVersions(ctx, conn)
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

func appliedVersions(ctx context.Context, conn *pgxpool.Conn) (map[string]bool, error) {
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
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
