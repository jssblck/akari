package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// reparseAdvisoryLockKey is the Postgres advisory-lock key that serializes a
// fleet-wide reparse across server instances. It is an arbitrary constant; the
// only requirement is that every akari-server agree on it (it does, since it is
// compiled in), so two instances reaching the startup epoch check at once cannot
// both reparse the same corpus. The value spells "akari" then "rp" in ASCII as a
// readable, collision-unlikely constant.
const reparseAdvisoryLockKey int64 = 0x616b6172695f7270

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
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, reparseAdvisoryLockKey).Scan(&got); err != nil {
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
// It is safe to call once; pass a live context (not a request context that may
// already be cancelled) so the unlock reaches Postgres even during shutdown.
func (l *ReparseLock) Release(ctx context.Context) {
	if l == nil || l.conn == nil {
		return
	}
	// Best-effort unlock: even if it fails, releasing the connection drops the
	// session and Postgres frees the advisory lock with it.
	_, _ = l.conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, reparseAdvisoryLockKey)
	l.conn.Release()
	l.conn = nil
}
