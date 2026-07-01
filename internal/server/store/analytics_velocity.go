package store

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
)

// VelocityStats answers "how fast does work move" over a scope: how long the agent takes
// to start replying after a prompt (the turn-cycle latency, at the median and the long
// tail), how slow the opening reply is on its own, and how densely messages and tool
// calls land during the time the session is actively working. It reads message
// timestamps, so it sees the same scoped cohort the rest of the Insights page does:
// sessions that started in the window.
type VelocityStats struct {
	ResponseP50       time.Duration // median prompt-to-first-reply latency, over every turn
	ResponseP90       time.Duration // 90th percentile, the slow-turn tail
	FirstResponseP50  time.Duration // median opening-turn latency (the prompt that pays the context load)
	MsgsPerActiveMin  float64       // messages per active minute (throughput, idle time excluded)
	ToolsPerActiveMin float64       // tool calls per active minute
	ActiveSeconds     float64       // total active time the rates divide by (the dead air between bursts removed)
	Turns             int           // prompt-to-reply pairs measured
	Sessions          int           // sessions that contributed at least one measured turn
}

// HasData reports whether the scope carried any measurable cadence, so the panel can show
// a note rather than a row of dashes and zeros on a window with no timed turns.
func (v VelocityStats) HasData() bool { return v.Turns > 0 || v.ActiveSeconds > 0 }

// HasThroughput reports whether there was any active time to divide by, so the panel can
// dash the per-minute rates rather than print a 0.0 that reads as a real measurement when
// the denominator is undefined (a single-message scope, or one whose every gap exceeds the
// idle cap).
func (v VelocityStats) HasThroughput() bool { return v.ActiveSeconds > 0 }

// activeGapSeconds is the idle threshold that separates "still working" from "walked
// away": a gap between two consecutive messages counts toward active time only when it is
// at most this long. A five-minute cap keeps a lunch break or an overnight pause out of
// the throughput denominator, so the rates read the pace of actual work rather than being
// diluted by the clock running while no one was there. The same gap idea sessionizes
// activity in the velocity literature; five minutes is the common default.
const activeGapSeconds = 300

// VelocityStats computes the scope's cadence figures for the Insights page. Latency, active
// time with message count, and tool count are three separate reads over the message timeline, so
// it wraps them in one repeatable-read, read-only snapshot: a concurrent projection update between
// them could otherwise combine active minutes from one message timeline with a tool count from
// another, making ToolsPerActiveMin and the headline cadence internally inconsistent. Insights
// threads its own snapshot through velocityStatsFrom; this is the standalone equivalent. The
// snapshot takes no row locks, so it never blocks ingest.
func (s *Store) VelocityStats(ctx context.Context, f AnalyticsFilter) (VelocityStats, error) {
	var out VelocityStats
	err := pgx.BeginTxFunc(ctx, s.Pool,
		pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			var err error
			out, err = s.velocityStatsFrom(ctx, tx, f)
			return err
		})
	if err != nil {
		return VelocityStats{}, fmt.Errorf("velocity stats snapshot: %w", err)
	}
	return out, nil
}

// velocityStatsFrom computes the scope's cadence figures in three reads over the message
// timeline.
//
// A turn is one prompt and the reply it draws: a running count of user-role messages
// labels every message with the turn it belongs to (the user message that opens the turn,
// then the assistant messages that answer it before the next prompt). Pure tool-result
// entries never reach the messages table (the Claude parser drops a user entry that
// carries only results), so a user-role message is a real prompt and the turn latency is
// the honest prompt-to-first-reply wait. The percentiles run over those per-turn waits;
// the first-response figure restricts to each session's opening turn, which pays the
// one-time context load and so reads slower than the steady state.
//
// Throughput divides the message and tool-call counts by active time: the summed gaps
// between consecutive messages, each gap kept only when it is short enough to be work
// rather than an idle pause (see activeGapSeconds). Both the counts and the active time
// read the stored projection rows as they are, replays included, so the numerator and the
// denominator stay on the same footing and the rate never skews from deduping one side.
func (s *Store) velocityStatsFrom(ctx context.Context, q querier, f AnalyticsFilter) (VelocityStats, error) {
	var v VelocityStats

	// Turn-cycle latency percentiles, plus the turn and session counts.
	//
	// The turn label counts user messages over EVERY message in the session, not just the
	// timestamped ones: an undated prompt must still open its own turn so a later reply
	// lands on that turn rather than being misattributed to the prior prompt as a false
	// latency. Timestamps are required only when deriving the per-turn user and reply
	// instants (the aggregates skip NULLs, so an undated turn falls out cleanly). The reply
	// instant is the FIRST assistant message by ordinal that carries a timestamp, so a
	// later assistant row whose clock drifted earlier cannot understate the latency.
	filter, args := f.clauseFor("s.started_at")
	var p50, p90, firstP50 float64
	if err := q.QueryRow(ctx,
		`WITH m AS (
		   SELECT m.session_id, m.ordinal, m.role, m.timestamp,
		          count(*) FILTER (WHERE m.role = 'user')
		            OVER (PARTITION BY m.session_id ORDER BY m.ordinal
		                  ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS turn
		     FROM messages m
		     JOIN sessions s ON s.id = m.session_id
		    WHERE TRUE`+filter+`
		 ),
		 user_turns AS (
		   SELECT session_id, turn, min(timestamp) FILTER (WHERE role = 'user') AS user_at
		     FROM m
		    WHERE turn >= 1
		    GROUP BY session_id, turn
		 ),
		 asst_turns AS (
		   -- The first assistant message by ordinal that carries a timestamp, picked with
		   -- DISTINCT ON so one long turn does not materialize every assistant timestamp into
		   -- an array just to read its head.
		   SELECT DISTINCT ON (session_id, turn) session_id, turn, timestamp AS asst_at
		     FROM m
		    WHERE turn >= 1 AND role = 'assistant' AND timestamp IS NOT NULL
		    ORDER BY session_id, turn, ordinal
		 ),
		 lat AS (
		   SELECT u.session_id, u.turn,
		          extract(epoch FROM (a.asst_at - u.user_at)) AS secs
		     FROM user_turns u
		     JOIN asst_turns a ON a.session_id = u.session_id AND a.turn = u.turn
		    WHERE u.user_at IS NOT NULL AND a.asst_at >= u.user_at
		 )
		 SELECT
		   coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY secs), 0),
		   coalesce(percentile_cont(0.9) WITHIN GROUP (ORDER BY secs), 0),
		   coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY secs) FILTER (WHERE turn = 1), 0),
		   count(*),
		   count(DISTINCT session_id)
		 FROM lat`, args...).Scan(&p50, &p90, &firstP50, &v.Turns, &v.Sessions); err != nil {
		return VelocityStats{}, fmt.Errorf("velocity latency percentiles: %w", err)
	}
	v.ResponseP50 = secondsToDuration(p50)
	v.ResponseP90 = secondsToDuration(p90)
	v.FirstResponseP50 = secondsToDuration(firstP50)

	// Active time and the message count that rides on it.
	filter, args = f.clauseFor("s.started_at")
	var msgCount int
	if err := q.QueryRow(ctx,
		fmt.Sprintf(`WITH g AS (
		   SELECT extract(epoch FROM (m.timestamp - lag(m.timestamp)
		            OVER (PARTITION BY m.session_id ORDER BY m.ordinal))) AS gap
		     FROM messages m
		     JOIN sessions s ON s.id = m.session_id
		    WHERE m.timestamp IS NOT NULL%s
		 )
		 SELECT count(*), coalesce(sum(gap) FILTER (WHERE gap > 0 AND gap <= %d), 0) FROM g`,
			filter, activeGapSeconds), args...).Scan(&msgCount, &v.ActiveSeconds); err != nil {
		return VelocityStats{}, fmt.Errorf("velocity active time: %w", err)
	}

	// Tool-call volume over the same cohort, for the tools-per-minute rate. The count is
	// restricted to calls whose owning message is timestamped, the same base the active
	// time and message count read, so the numerator and denominator describe one timeline.
	filter, args = f.clauseFor("s.started_at")
	var toolCount int
	if err := q.QueryRow(ctx,
		`SELECT count(*)
		   FROM tool_calls tc
		   JOIN messages m ON m.session_id = tc.session_id AND m.ordinal = tc.message_ordinal
		   JOIN sessions s ON s.id = tc.session_id
		  WHERE m.timestamp IS NOT NULL`+filter, args...).Scan(&toolCount); err != nil {
		return VelocityStats{}, fmt.Errorf("velocity tool count: %w", err)
	}

	if v.ActiveSeconds > 0 {
		activeMin := v.ActiveSeconds / 60
		v.MsgsPerActiveMin = float64(msgCount) / activeMin
		v.ToolsPerActiveMin = float64(toolCount) / activeMin
	}
	return v, nil
}

// secondsToDuration converts a percentile's float seconds into a Duration, rounding to
// the nearest millisecond so the stored value carries no spurious sub-millisecond noise
// from the floating-point epoch math.
func secondsToDuration(secs float64) time.Duration {
	return time.Duration(math.Round(secs*1000)) * time.Millisecond
}
