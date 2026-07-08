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

// velocityStatsFrom computes the scope's cadence figures in two reads over the velocity
// rollups; messages itself never enters the read path.
//
// A turn is one prompt and the reply it draws. The turn labelling (a running count of
// user-role messages, so an undated prompt still opens its own turn and a later reply
// cannot misattribute; the reply instant is the first timestamped assistant message by
// ordinal) happens once, at write time, in deriveSessionRollupsTx: session_turns stores
// one row per measured turn with its float-second latency verbatim, so the percentiles
// here interpolate the same values the per-read fold produced. The first-response figure
// restricts to each session's opening turn (turn = 1), which pays the one-time context
// load and so reads slower than the steady state.
//
// Throughput divides the message and tool-call counts by active time: the summed gaps
// between consecutive messages, each gap kept only when it is short enough to be work
// rather than an idle pause (see activeGapSeconds). session_activity_hourly carries all
// three per (session, UTC day, hour), derived from the stored projection rows as they
// are, replays included, so the numerator and the denominator stay on the same footing
// and the rate never skews from deduping one side.
func (s *Store) velocityStatsFrom(ctx context.Context, q querier, f AnalyticsFilter) (VelocityStats, error) {
	var v VelocityStats

	// Turn-cycle latency percentiles, plus the turn and session counts.
	filter, args := f.clauseFor("s.started_at")
	var p50, p90, firstP50 float64
	if err := q.QueryRow(ctx,
		`SELECT
		   coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY st.response_secs), 0),
		   coalesce(percentile_cont(0.9) WITHIN GROUP (ORDER BY st.response_secs), 0),
		   coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY st.response_secs) FILTER (WHERE st.turn = 1), 0),
		   count(*),
		   count(DISTINCT st.session_id)
		 FROM session_turns st
		 JOIN sessions s ON s.id = st.session_id
		 WHERE TRUE`+filter, args...).Scan(&p50, &p90, &firstP50, &v.Turns, &v.Sessions); err != nil {
		return VelocityStats{}, fmt.Errorf("velocity latency percentiles: %w", err)
	}
	v.ResponseP50 = secondsToDuration(p50)
	v.ResponseP90 = secondsToDuration(p90)
	v.FirstResponseP50 = secondsToDuration(firstP50)

	// Active time and the message and tool counts that ride on it, one sum over the
	// activity rollup (the old read needed a lag() scan of messages plus a tool_calls join;
	// the rollup already paid both at write time).
	filter, args = f.clauseFor("s.started_at")
	var msgCount, toolCount int
	if err := q.QueryRow(ctx,
		`SELECT coalesce(sum(ah.messages), 0)::bigint,
		        coalesce(sum(ah.tool_calls), 0)::bigint,
		        coalesce(sum(ah.active_seconds), 0)
		   FROM session_activity_hourly ah
		   JOIN sessions s ON s.id = ah.session_id
		  WHERE TRUE`+filter, args...).Scan(&msgCount, &toolCount, &v.ActiveSeconds); err != nil {
		return VelocityStats{}, fmt.Errorf("velocity active time: %w", err)
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
