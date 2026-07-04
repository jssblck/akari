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

// parseFailurePinned returns the SQL predicate matching sessions whose last
// deterministic parse failure still covers their exact current state: the same
// bytes, at the running epoch ($1). Those are the only rebuilds worth
// skipping, because identical input at the same parser fails identically; new
// bytes or a new epoch make the failure retryable again. DueSessions,
// EpochStaleCount, EpochStaleExists, and the analytics snapshot gate all embed
// its negation, so "due", "counted stale", and "gated" can never drift apart:
// a session the worker can rebuild at the running epoch is always visible to
// the gates, and one it cannot is invisible to all of them. tbl prefixes the
// column references (a table alias like "sr." or empty).
func parseFailurePinned(tbl string) string {
	return fmt.Sprintf(
		`(%[1]sparse_error <> '' AND %[1]sparse_error_epoch = $1 AND %[1]sparse_error_byte_len = %[1]sbyte_len)`,
		tbl)
}

// DueSessions returns up to limit sessions due for a rebuild, strictly after
// the afterID keyset cursor, in id order. A session is due when the last
// successful rebuild did not cover its current bytes (parsed_byte_len <>
// byte_len) or ran at an earlier epoch. The epoch comparison is deliberately
// monotonic (parser_epoch <= running, never <>): during a rolling deploy the
// old binary would otherwise see the new binary's stamps as "different" and
// rebuild them with the old parser, downgrading projections the fleet already
// advanced. Rows stamped ahead of the running epoch are left alone entirely,
// even when byte-dirty; the instance running the newer binary picks them up on
// its next wake or tick. The parser_epoch < $1 half is a single btree range,
// and the byte comparison is carried by the partial index on dirty rows
// (near-empty in steady state), so the every-chunk wake never scans the
// corpus.
//
// A session whose last attempt failed deterministically is skipped only while
// the recorded failure matches its current bytes and the running epoch (see
// parseFailurePinned): retrying identical input at the same epoch would fail
// identically, but new bytes or a new epoch retry it. Operational failures
// record nothing and stay due for the next drain.
func (s *Store) DueSessions(ctx context.Context, epoch int, afterID int64, limit int) ([]DueSession, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT sr.session_id, s.agent, sr.parser_epoch < $1 AS epoch_stale
		   FROM session_raw sr
		   JOIN sessions s ON s.id = sr.session_id
		  WHERE sr.session_id > $2
		    AND sr.parser_epoch <= $1
		    AND (sr.parsed_byte_len <> sr.byte_len OR sr.parser_epoch < $1)
		    AND NOT `+parseFailurePinned("sr.")+`
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

// EpochStaleCount counts the sessions whose projection was last rebuilt behind
// the running epoch and can still be rebuilt at it. A nonzero count means a
// fleet rebuild is draining (a deploy with a bumped epoch, a first boot after
// the pipeline migration, or an operator-marked scope), which is what the
// parsed-page gate and the rebuild progress bar key on. Behind only, matching
// DueSessions: a row stamped ahead of the running epoch (a rolled-back binary
// looking at a newer stamp) is not this binary's work, so counting it would
// wedge the gate on sessions the drain will never touch. A session whose parse
// already failed at the running epoch over its current bytes is excluded the
// same way DueSessions skips it (parseFailurePinned): the drain can never
// advance it, so counting it would hold the gate and the bar short of done
// forever. New bytes un-pin the failure and the session counts (and is due)
// again.
func (s *Store) EpochStaleCount(ctx context.Context, epoch int) (int, error) {
	var n int
	if err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM session_raw
		  WHERE parser_epoch < $1
		    AND NOT `+parseFailurePinned(""), epoch).Scan(&n); err != nil {
		return 0, fmt.Errorf("count epoch-stale sessions: %w", err)
	}
	return n, nil
}

// EpochStaleExists answers whether any session is epoch-stale, with the same
// predicate as EpochStaleCount but stopping at the first hit. It backs the
// cross-instance fleet gate (Worker.FleetStatus), which only needs the
// boolean; counting the whole backlog on a page request would pay for
// precision the gate throws away.
func (s *Store) EpochStaleExists(ctx context.Context, epoch int) (bool, error) {
	var stale bool
	if err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM session_raw
		    WHERE parser_epoch < $1
		      AND NOT `+parseFailurePinned("")+`
		 )`, epoch).Scan(&stale); err != nil {
		return false, fmt.Errorf("check epoch-stale sessions: %w", err)
	}
	return stale, nil
}

// MarkEpochStale forces a rebuild of every session (or one agent's sessions) by
// resetting their stored parser_epoch to 0, which is behind every real epoch.
// It is how the admin Reparse button and the `akari-server reparse` CLI
// trigger a fleet rebuild: mark the scope due, wake the worker, and the
// ordinary drain does the rest. The failure markers reset too: the operator
// asked for the whole scope, so previously failed sessions get one fresh
// attempt (and re-record their failure if the bytes still do not parse). It
// returns how many sessions were marked.
func (s *Store) MarkEpochStale(ctx context.Context, agent string) (int, error) {
	q := `UPDATE session_raw sr SET parser_epoch = 0, parse_error_epoch = 0, parse_error_byte_len = 0`
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
