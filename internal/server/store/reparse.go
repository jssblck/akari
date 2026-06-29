package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// reparseLockKey is the second key of the per-database advisory lock that
// serializes a fleet-wide reparse across server instances. The first key is
// hashtext(current_database()), which scopes the lock to one akari database: every
// instance of a deployment shares that database and so contends on the same lock,
// while two akari databases that happen to share a Postgres cluster (advisory
// locks are otherwise cluster-global) get independent locks and never block each
// other. The value spells "akrp" in ASCII as a readable, collision-unlikely
// constant; it is an int32 because the two-key advisory-lock form takes int4 keys.
const reparseLockKey int32 = 0x616b7270

// ReparsedEpoch reads the parser epoch the stored projection was last rebuilt
// under. A fresh database returns 0 (the column default), which differs from
// parse.Epoch so the server reparses on first start and converges.
func (s *Store) ReparsedEpoch(ctx context.Context) (int, error) {
	var epoch int
	if err := s.Pool.QueryRow(ctx, `SELECT reparsed_epoch FROM parse_meta WHERE id = TRUE`).Scan(&epoch); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The migration seeds the singleton row, so a missing row means the
			// migration has not run yet; treat it as "never reparsed" rather than an
			// error so the caller's epoch comparison still triggers a reparse.
			return 0, nil
		}
		return 0, fmt.Errorf("read reparsed_epoch: %w", err)
	}
	return epoch, nil
}

// ReparseScope reports how many sessions a reparse will cover and the largest id
// among them, optionally filtered to one agent. The reparse loop pages through ids
// up to maxID, so a session ingested after the scope is read is left to the live
// parse path rather than swelling the count. total drives the progress bar's
// denominator; maxID bounds the keyset paging.
func (s *Store) ReparseScope(ctx context.Context, agent string) (total int, maxID int64, err error) {
	q := "SELECT count(*), coalesce(max(id), 0) FROM sessions"
	var args []any
	if agent != "" {
		q += " WHERE agent = $1"
		args = append(args, agent)
	}
	if err := s.Pool.QueryRow(ctx, q, args...).Scan(&total, &maxID); err != nil {
		return 0, 0, fmt.Errorf("scope sessions for reparse (agent=%q): %w", agent, err)
	}
	return total, maxID, nil
}

// SessionsForReparsePage returns the next page of reparse targets: the sessions
// with id in (afterID, maxID], ordered by id, capped at limit. Keyset paging on the
// primary key keeps each query bounded and lets the reparse loop hold only one page
// (plus the current session) in memory, rather than the whole corpus. An empty
// result means the scope is exhausted.
func (s *Store) SessionsForReparsePage(ctx context.Context, agent string, afterID, maxID int64, limit int) ([]ReparseTarget, error) {
	q := "SELECT id, agent FROM sessions WHERE id > $1 AND id <= $2"
	args := []any{afterID, maxID}
	if agent != "" {
		q += " AND agent = $3"
		args = append(args, agent)
	}
	q += " ORDER BY id LIMIT " + strconv.Itoa(limit)
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("page sessions for reparse (agent=%q, after=%d): %w", agent, afterID, err)
	}
	defer rows.Close()
	var out []ReparseTarget
	for rows.Next() {
		var t ReparseTarget
		if err := rows.Scan(&t.ID, &t.Agent); err != nil {
			return nil, fmt.Errorf("scan reparse target: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reparse targets: %w", err)
	}
	return out, nil
}

// ReparseLockHeld reports whether any session currently holds the reparse advisory
// lock for this database. It is how a server instance that is not itself reparsing
// learns that another instance is, so it can gate its parsed UI for the duration:
// the advisory lock is the authoritative cross-process "a reparse is running"
// signal, and it clears automatically if the holder crashes. The two-key advisory
// lock records classid = first key, objid = second key, objsubid = 2; classid is an
// oid, so the first key is compared as one (the cast also carries a negative
// hashtext result through as its unsigned bit pattern, the way Postgres stores it).
func (s *Store) ReparseLockHeld(ctx context.Context) (bool, error) {
	var held bool
	if err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM pg_locks
		   WHERE locktype = 'advisory'
		     AND classid = hashtext(current_database())::oid
		     AND objid = $1::oid
		     AND objsubid = 2
		     AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		 )`, reparseLockKey).Scan(&held); err != nil {
		return false, fmt.Errorf("check reparse lock: %w", err)
	}
	return held, nil
}

// SetReparsedEpoch records that the whole corpus has been reparsed under the given
// epoch. It is written only after a full (all-agents) reparse completes, so the
// next startup sees the epochs match and does not reparse again.
func (s *Store) SetReparsedEpoch(ctx context.Context, epoch int) error {
	if _, err := s.Pool.Exec(ctx,
		`UPDATE parse_meta SET reparsed_epoch = $1, updated_at = now() WHERE id = TRUE`, epoch); err != nil {
		return fmt.Errorf("set reparsed_epoch to %d: %w", epoch, err)
	}
	return nil
}

// ReparseLock holds the session-scoped advisory lock that serializes a reparse
// across processes. It pins a dedicated pooled connection for the lock's lifetime,
// because a Postgres advisory lock is tied to the session that took it; Release
// unlocks and returns the connection to the pool.
type ReparseLock struct {
	conn *pgxpool.Conn
}

// AcquireReparseLock tries to take the fleet-wide reparse advisory lock without
// blocking. It returns (lock, true, nil) when the lock was acquired and the caller
// owns it until Release, or (nil, false, nil) when another instance already holds
// it, in which case the caller should skip its reparse. The lock is advisory and
// session-scoped, so a crashed holder releases it automatically when its
// connection drops.
func (s *Store) AcquireReparseLock(ctx context.Context) (*ReparseLock, bool, error) {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire reparse-lock connection: %w", err)
	}
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext(current_database()), $1)`, reparseLockKey).Scan(&got); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("pg_try_advisory_lock: %w", err)
	}
	if !got {
		conn.Release()
		return nil, false, nil
	}
	return &ReparseLock{conn: conn}, true, nil
}

// Release unlocks the advisory lock and returns the pinned connection to the pool.
// It is safe to call once. Pass a bounded context (the caller derives one that is
// detached from request/shutdown cancellation but capped) so a stuck unlock cannot
// block shutdown indefinitely.
//
// If the unlock fails, the session may still hold the lock, so the connection is
// destroyed (hijacked out of the pool and closed) rather than returned: ending the
// session is what frees the advisory lock, and it stops a future pool user from
// inheriting a held reparse lock and wedging all later reparses.
func (l *ReparseLock) Release(ctx context.Context) {
	if l == nil || l.conn == nil {
		return
	}
	conn := l.conn
	l.conn = nil
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock(hashtext(current_database()), $1)`, reparseLockKey); err != nil {
		if raw := conn.Hijack(); raw != nil {
			_ = raw.Close(ctx)
		}
		return
	}
	conn.Release()
}
