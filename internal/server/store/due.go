package store

import (
	"context"
	"fmt"
)

// DueSession is one session the parse worker should rebuild: its projection is
// behind its raw bytes or behind the running parser epoch. EpochStale says the
// epoch is what made it due (possibly alongside new bytes), which is the subset
// a fleet rebuild's progress counts.
type DueSession struct {
	ID         int64
	Agent      string
	EpochStale bool
}

// DueSessions returns up to limit sessions due for a rebuild, strictly after
// the afterID keyset cursor, in id order. A session is due when the last
// successful rebuild did not cover its current bytes (parsed_byte_len <>
// byte_len) or ran at a different epoch. The epoch inequality is written as two
// range predicates so the parser_epoch btree serves it; the byte comparison has
// no index, but the epoch index carries the fleet-rebuild case and the byte
// case is bounded by the live-session set the worker was just woken for.
//
// The scan deliberately has no "attempted" filter: a deterministic parse
// failure stamps the session's bookkeeping as consumed (see RebuildSession), so
// it leaves this set on its own, and an operational failure should be retried.
// The worker dedups within one drain pass to avoid hot-looping on a session
// that fails operationally.
func (s *Store) DueSessions(ctx context.Context, epoch int, afterID int64, limit int) ([]DueSession, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT sr.session_id, s.agent, sr.parser_epoch <> $1 AS epoch_stale
		   FROM session_raw sr
		   JOIN sessions s ON s.id = sr.session_id
		  WHERE sr.session_id > $2
		    AND (sr.parsed_byte_len <> sr.byte_len
		         OR sr.parser_epoch < $1 OR sr.parser_epoch > $1)
		  ORDER BY sr.session_id
		  LIMIT $3`, epoch, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("select due sessions: %w", err)
	}
	defer rows.Close()
	var out []DueSession
	for rows.Next() {
		var d DueSession
		if err := rows.Scan(&d.ID, &d.Agent, &d.EpochStale); err != nil {
			return nil, fmt.Errorf("scan due session: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate due sessions: %w", err)
	}
	return out, nil
}

// EpochStaleCount counts the sessions whose projection was last rebuilt at a
// different epoch than the running one. A nonzero count means a fleet rebuild
// is draining (a deploy with a bumped epoch, a first boot after the pipeline
// migration, or an operator-marked scope), which is what the parsed-page gate
// and the rebuild progress bar key on. The inequality is written as two ranges
// so the parser_epoch btree serves it; in steady state both ranges are empty.
func (s *Store) EpochStaleCount(ctx context.Context, epoch int) (int, error) {
	var n int
	if err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM session_raw
		  WHERE parser_epoch < $1 OR parser_epoch > $1`, epoch).Scan(&n); err != nil {
		return 0, fmt.Errorf("count epoch-stale sessions: %w", err)
	}
	return n, nil
}

// MarkEpochStale forces a rebuild of every session (or one agent's sessions) by
// resetting their stored parser_epoch to 0, which differs from every real
// epoch. It is how the admin Reparse button and the `akari-server reparse` CLI
// trigger a fleet rebuild: mark the scope due, wake the worker, and the
// ordinary drain does the rest. It returns how many sessions were marked.
func (s *Store) MarkEpochStale(ctx context.Context, agent string) (int, error) {
	q := `UPDATE session_raw sr SET parser_epoch = 0`
	var args []any
	if agent != "" {
		q += ` FROM sessions s WHERE s.id = sr.session_id AND s.agent = $1`
		args = append(args, agent)
	}
	tag, err := s.Pool.Exec(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("mark sessions epoch-stale (agent=%q): %w", agent, err)
	}
	return int(tag.RowsAffected()), nil
}
