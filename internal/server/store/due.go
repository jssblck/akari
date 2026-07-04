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

// attemptedEpoch returns the SQL expression for the newest epoch that has
// fully dealt with a session as it currently stands: the epoch of the last
// successful rebuild, raised to the failure epoch while a recorded
// deterministic failure still covers the session's current bytes. A session is
// epoch-stale exactly when this value is behind the running epoch, which is
// the one predicate every staleness surface shares: the due scan's epoch
// branch, EpochStaleCount, EpochStaleExists, and the analytics snapshot gate.
// Sharing it keeps "due", "counted stale", and "gated" from ever drifting
// apart, and folding the failure pin into the indexed value (rather than
// filtering pinned rows out of a raw parser_epoch range) keeps sessions whose
// parse failed at the running epoch OUT of the range the hot probes scan, so
// their cost tracks the actual backlog, not the corpus's accumulated failure
// history. New bytes break the byte match, the expression falls back to
// parser_epoch, and the session re-enters the range (and the due set) at once.
//
// The expression matches idx_session_raw_attempted_epoch (migration 0044);
// keep the two in sync or the planner falls back to a seq scan. tbl prefixes
// the column references (a table alias like "sr." or empty).
func attemptedEpoch(tbl string) string {
	return fmt.Sprintf(
		`(CASE WHEN %[1]sparse_error <> '' AND %[1]sparse_error_byte_len = %[1]sbyte_len
		       THEN GREATEST(%[1]sparser_epoch, %[1]sparse_error_epoch)
		       ELSE %[1]sparser_epoch END)`, tbl)
}

// DueSessions returns up to limit sessions due for a rebuild, in no particular
// order. A session is due when the last successful rebuild did not cover its
// current bytes (parsed_byte_len <> byte_len) or ran at an earlier epoch
// (attemptedEpoch behind the running one). The epoch comparisons are
// deliberately monotonic (behind-only, never <>): during a rolling deploy the
// old binary would otherwise see the new binary's stamps as "different" and
// rebuild them with the old parser, downgrading projections the fleet already
// advanced. Rows stamped ahead of the running epoch are left alone entirely,
// even when byte-dirty; the instance running the newer binary picks them up
// on its next wake or tick.
//
// There is no page cursor because none is needed: every session a drain
// processes leaves this scan's result set before the next page is fetched. A
// successful rebuild stamps it current, a deterministic parser failure pins it
// (parse_error markers covering its bytes at the running epoch), and an
// operational failure parks it on a retry backoff (RecordRebuildBackoff; the
// worker treats a failure of THAT write as fatal to the drain, which is what
// makes this guarantee hold). So "the next page" is simply the first limit
// ready sessions again, and a drain loops until the page comes back empty.
//
// The query is a UNION of three arms rather than one OR so that each arm
// terminates at its own LIMIT on its own partial index, whatever the planner's
// current statistics say (a single OR-ed predicate under an outer LIMIT has
// been observed to plan as a full index walk). Each arm reads an index that
// already excludes what the drain must not visit, so rows parked on a future
// retry cost nothing here however many the corpus accumulates:
//
//   - dirty_ready: byte-dirty, undeferred (the every-chunk wake's arm);
//   - epoch_ready: attemptedEpoch behind the running epoch, undeferred (the
//     fleet-rebuild arm; the expression folds the failed-parse pin in, so a
//     session whose failure covers its current bytes at the running epoch or
//     above is absent: retrying identical input would fail identically, and an
//     attempt recorded ahead belongs to a newer binary);
//   - retry_elapsed: one range over deferral times picks up exactly the parked
//     retries whose backoff has elapsed. This arm needs no dirty-or-stale
//     recheck because a deferral only exists on a session that was due when
//     its rebuild failed, and everything that advances a session clears the
//     deferral in the same statement.
//
// The epoch-staleness gates deliberately do NOT honor the deferral: a
// backing-off rebuild is deferred, not cancelled, so the corpus is still mixed
// and the gate staying up is the honest answer (the safe direction of
// asymmetry; the gates must never read done while the scan still has work).
func (s *Store) DueSessions(ctx context.Context, epoch int, limit int) ([]DueSession, error) {
	arm := func(where string) string {
		return `(SELECT sr.session_id, ` + attemptedEpoch("sr.") + ` < $1 AS epoch_stale
		           FROM session_raw sr
		          WHERE sr.parser_epoch <= $1
		            AND (sr.parse_error = ''
		                 OR sr.parse_error_byte_len <> sr.byte_len
		                 OR sr.parse_error_epoch < $1)
		            AND ` + where + `
		          LIMIT $2)`
	}
	rows, err := s.Pool.Query(ctx,
		`SELECT u.session_id, s.agent, u.epoch_stale
		   FROM (`+
			arm(`sr.parse_retry_at IS NULL AND sr.parsed_byte_len <> sr.byte_len`)+
			` UNION `+
			arm(`sr.parse_retry_at IS NULL AND `+attemptedEpoch("sr.")+` < $1`)+
			` UNION `+
			arm(`sr.parse_retry_at <= now()`)+
			`) u
		   JOIN sessions s ON s.id = u.session_id
		  LIMIT $2`, epoch, limit)
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
// the running epoch and can still be rebuilt at it (attemptedEpoch, which
// folds in both the monotonic behind-only comparison and the failed-parse
// pin). A nonzero count means a fleet rebuild is draining (a deploy with a
// bumped epoch, a first boot after the pipeline migration, or an
// operator-marked scope), which is what the parsed-page gate and the rebuild
// progress bar key on. A row stamped or pinned at the running epoch or ahead
// is not this binary's work: counting it would wedge the gate and the bar on
// sessions the drain will never advance.
func (s *Store) EpochStaleCount(ctx context.Context, epoch int) (int, error) {
	var n int
	if err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM session_raw
		  WHERE `+attemptedEpoch("")+` < $1`, epoch).Scan(&n); err != nil {
		return 0, fmt.Errorf("count epoch-stale sessions: %w", err)
	}
	return n, nil
}

// EpochStaleReadyCount counts the epoch-stale sessions a drain can rebuild
// RIGHT NOW: EpochStaleCount minus the rows parked on a future retry backoff.
// It is the drain's opening count (fleet mode and the progress denominator),
// which runs on every chunk wake: counting the honest total there would pay
// O(deferred backlog) per append just to decide there is nothing to do, since
// persistent operational failures stay epoch-stale for as long as they keep
// failing. The two arms mirror the due scan's, so each lands on a ready index
// (epoch_ready, retry_elapsed) and parked rows are never visited. The fleet
// GATE keeps the honest total-side answer (EpochStaleExists over the full
// index): a deferred rebuild still makes the corpus mixed; it is only not this
// drain's workload.
func (s *Store) EpochStaleReadyCount(ctx context.Context, epoch int) (int, error) {
	var n int
	if err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM session_raw
		  WHERE (parse_retry_at IS NULL AND `+attemptedEpoch("")+` < $1)
		     OR (parse_retry_at <= now() AND `+attemptedEpoch("")+` < $1)`, epoch).Scan(&n); err != nil {
		return 0, fmt.Errorf("count ready epoch-stale sessions: %w", err)
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
		    WHERE `+attemptedEpoch("")+` < $1
		 )`, epoch).Scan(&stale); err != nil {
		return false, fmt.Errorf("check epoch-stale sessions: %w", err)
	}
	return stale, nil
}

// RecordRebuildBackoff defers a session's next rebuild attempt after an
// operational failure (the rebuild rolled back; the session is still due). It
// runs in its own transaction, after the failed one, and is best-effort: if
// this write fails too, the session simply retries on the next wake, which is
// the pre-backoff behavior. The deferral doubles on consecutive failures, from
// 30 seconds to a one-hour ceiling, so a failure that does not clear on its
// own (a CAS blob the client never uploaded) costs the drain one attempt per
// backoff window instead of one per chunk wake. Everything that changes the
// situation clears the marker for an immediate retry: a successful rebuild, a
// recorded deterministic failure, new bytes, a raw reset, an operator reparse.
func (s *Store) RecordRebuildBackoff(ctx context.Context, sessionID int64) error {
	// SET expressions all read the OLD row, so both columns derive the new
	// backoff from the same pre-update value.
	if _, err := s.Pool.Exec(ctx,
		`UPDATE session_raw
		    SET parse_retry_backoff_secs = LEAST(GREATEST(parse_retry_backoff_secs * 2, 30), 3600),
		        parse_retry_at = now() + make_interval(secs => LEAST(GREATEST(parse_retry_backoff_secs * 2, 30), 3600))
		  WHERE session_id = $1`, sessionID); err != nil {
		return fmt.Errorf("record rebuild backoff for session %d: %w", sessionID, err)
	}
	return nil
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
	q := `UPDATE session_raw sr SET parser_epoch = 0, parse_error_epoch = 0, parse_error_byte_len = 0,
	                                parse_retry_at = NULL, parse_retry_backoff_secs = 0`
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
