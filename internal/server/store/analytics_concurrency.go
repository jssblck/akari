package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConcurrencyStats answers "how many sessions ran at once" over a scope: the fleet's
// peak overlap and when it happened, the single busiest user's peak, and the average
// concurrency across the active span. It reads from session start/end spans, so it sees
// the same scoped sessions the rest of the Insights page does. A session with no parsed
// start or end (or an end before its start) carries no measurable span and sits out.
type ConcurrencyStats struct {
	FleetPeak       int       // most sessions active simultaneously, anywhere in scope
	FleetPeakAt     time.Time // when the fleet peak was first reached
	BusiestUser     string    // the user whose own sessions overlapped the most
	BusiestUserPeak int       // that user's peak simultaneous sessions
	AvgConcurrent   float64   // total active session-time over the span the sessions cover
	Sessions        int       // sessions with a measurable span in scope
}

// HasData reports whether any scoped session had a measurable span, so the panel can
// show an empty state rather than a peak of zero.
func (c ConcurrencyStats) HasData() bool { return c.Sessions > 0 }

// spanFilter is the shared predicate that keeps the sweep honest: a session counts
// toward concurrency only with a parsed start and end and a non-negative duration. It
// sits at the front of every concurrency CTE so the three queries measure the same set.
const spanFilter = " s.started_at IS NOT NULL AND s.ended_at IS NOT NULL AND s.ended_at >= s.started_at"

// ConcurrencyStats computes the scope's overlap figures for the Insights page. Fleet peak, busiest
// user, and the average plus session count are three separate reads over the same span set, so it
// wraps them in one repeatable-read, read-only snapshot: a concurrent ingest between the reads could
// otherwise return a peak from one cohort with a session count and average from another. Insights
// threads its own snapshot through concurrencyStatsFrom; this is the standalone equivalent. The
// snapshot takes no row locks, so it never blocks ingest.
func (s *Store) ConcurrencyStats(ctx context.Context, f AnalyticsFilter) (ConcurrencyStats, error) {
	var out ConcurrencyStats
	err := pgx.BeginTxFunc(ctx, s.Pool,
		pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			var err error
			out, err = s.concurrencyStatsFrom(ctx, tx, f)
			return err
		})
	if err != nil {
		return ConcurrencyStats{}, fmt.Errorf("concurrency stats snapshot: %w", err)
	}
	return out, nil
}

// concurrencyStatsFrom computes the scope's overlap figures with a sweep line over the
// session spans: each session contributes a +1 at its start and a -1 at its end, and the
// running sum over those events ordered by time is the live concurrency. Ties order
// starts before ends (delta DESC), so two sessions that touch at an instant register the
// momentary overlap, the standard "rooms needed" reading. The fleet peak is the running
// max; the per-user peak runs the same sweep partitioned by user; the average is the
// total active session-time divided by the span the sessions actually cover, so a window
// with a short burst of activity reads its average over that burst, not over the dead air
// around it.
func (s *Store) concurrencyStatsFrom(ctx context.Context, q querier, f AnalyticsFilter) (ConcurrencyStats, error) {
	var cs ConcurrencyStats

	// Fleet peak and the instant it was first reached.
	filter, args := f.clauseFor("s.started_at")
	var peakAt *time.Time
	err := q.QueryRow(ctx,
		`WITH spans AS (
		   SELECT s.started_at AS a, s.ended_at AS b
		     FROM sessions s WHERE`+spanFilter+filter+`
		 ),
		 ev AS (
		   SELECT a AS t, 1 AS d FROM spans
		   UNION ALL
		   SELECT b AS t, -1 AS d FROM spans
		 ),
		 run AS (
		   SELECT t, sum(d) OVER (ORDER BY t, d DESC) AS c FROM ev
		 )
		 SELECT c, t FROM run ORDER BY c DESC, t LIMIT 1`, args...).Scan(&cs.FleetPeak, &peakAt)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ConcurrencyStats{}, fmt.Errorf("fleet concurrency peak: %w", err)
	}
	if peakAt != nil {
		cs.FleetPeakAt = *peakAt
	}

	// The single busiest user: the same sweep partitioned by user, then the highest peak.
	filter, args = f.clauseFor("s.started_at")
	var user *string
	var userPeak int
	err = q.QueryRow(ctx,
		`WITH spans AS (
		   SELECT s.user_id AS u, s.started_at AS a, s.ended_at AS b
		     FROM sessions s WHERE`+spanFilter+filter+`
		 ),
		 ev AS (
		   SELECT u, a AS t, 1 AS d FROM spans
		   UNION ALL
		   SELECT u, b AS t, -1 AS d FROM spans
		 ),
		 run AS (
		   SELECT u, sum(d) OVER (PARTITION BY u ORDER BY t, d DESC) AS c FROM ev
		 ),
		 peaks AS (
		   SELECT u, max(c) AS peak FROM run GROUP BY u
		 )
		 SELECT us.username, p.peak
		   FROM peaks p JOIN users us ON us.id = p.u
		  ORDER BY p.peak DESC, us.username LIMIT 1`, args...).Scan(&user, &userPeak)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ConcurrencyStats{}, fmt.Errorf("busiest user concurrency: %w", err)
	}
	if user != nil {
		cs.BusiestUser, cs.BusiestUserPeak = *user, userPeak
	}

	// Session count, total active session-seconds, and the covered span for the average.
	filter, args = f.clauseFor("s.started_at")
	var sumSecs float64
	var minStart, maxEnd *time.Time
	if err := q.QueryRow(ctx,
		`SELECT count(*),
		        coalesce(sum(extract(epoch FROM (s.ended_at - s.started_at))), 0),
		        min(s.started_at), max(s.ended_at)
		   FROM sessions s WHERE`+spanFilter+filter, args...).Scan(&cs.Sessions, &sumSecs, &minStart, &maxEnd); err != nil {
		return ConcurrencyStats{}, fmt.Errorf("concurrency span aggregate: %w", err)
	}
	if minStart != nil && maxEnd != nil {
		if spanSecs := maxEnd.Sub(*minStart).Seconds(); spanSecs > 0 {
			cs.AvgConcurrent = sumSecs / spanSecs
		} else if cs.Sessions > 0 {
			// Every session collapsed to one instant (zero-length spans). The integral
			// reading is undefined, so report the count of those coincident sessions.
			cs.AvgConcurrent = float64(cs.Sessions)
		}
	}
	return cs, nil
}
