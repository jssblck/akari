package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// dedupToolCallsPartition is the PARTITION BY that collapses replayed tool-call rows in a
// COHORT query (one spanning many sessions). A resumed or compacted transcript re-emits a
// call under new ordinals, so rows sharing (session, id, tool, input, result) are one call.
// session_id leads the partition because a call id is only unique within its session; the
// CASE gives an id-less row its own per-row key ("ordinal:index") in a separate column, so
// a real id can never collide with a synthetic one. It mirrors the per-session dedup in
// gatherSignalFacts (signals.go), with session_id added for the cross-session scope, and is
// shared by the tool and churn cohort queries so the two cannot drift apart.
const dedupToolCallsPartition = `session_id, call_uid,
	CASE WHEN call_uid IS NULL THEN message_ordinal::text || ':' || call_index END,
	tool_name, coalesce(input_sha256, ''), coalesce(result_status, '')`

// maxToolBars caps the per-tool mix at the busiest tools, so a fleet with a long tail of
// rarely-used tools still reads as a short, legible bar list. The fleet totals (calls,
// failures, error rate) are summed over EVERY tool, not just the ones shown, so the
// headline figures cover the whole fleet even when the bar list is clipped.
const maxToolBars = 10

// ToolStat is one tool's volume and reliability over a scope: how many times it ran and
// how many of those runs failed. Calls and Failures are deduped (a replayed transcript
// re-emits prior tool calls, so the raw rows over-count), the same dedup the per-session
// signals apply, so the fleet figures reconcile with the session tiles.
type ToolStat struct {
	Name     string
	Calls    int
	Failures int
}

// ErrorRate is the tool's failure share, 0 when it never ran.
func (t ToolStat) ErrorRate() float64 {
	if t.Calls == 0 {
		return 0
	}
	return float64(t.Failures) / float64(t.Calls)
}

// ToolStats is the fleet tool picture for a scope: the overall call and failure volume,
// the prompt count the tools-per-turn rate divides by, and the busiest tools with their
// own reliability. It answers "how much tool work happens, how reliable is it, and which
// tools carry it" over the same cohort the rest of the Insights page reads.
type ToolStats struct {
	TotalCalls    int
	TotalFailures int
	Turns         int        // human prompts across the cohort (sum of user_message_count)
	Tools         []ToolStat // the busiest tools, most calls first (see maxToolBars)
	Clipped       int        // tools beyond the shown list, folded into the totals but not the bars
}

// HasData reports whether the scope ran any tools, so the panel can show a note rather
// than an empty bar list for a pure-conversation window.
func (t ToolStats) HasData() bool { return t.TotalCalls > 0 }

// ErrorRate is the fleet failure share across every tool, 0 when nothing ran.
func (t ToolStats) ErrorRate() float64 {
	if t.TotalCalls == 0 {
		return 0
	}
	return float64(t.TotalFailures) / float64(t.TotalCalls)
}

// ToolsPerTurn is how many tool calls the fleet ran per human prompt, 0 when the cohort
// has no prompts (a window of pure automation). The denominator is every prompt in the
// cohort, so a conversation that needed no tools pulls the rate down rather than sitting
// outside the average.
func (t ToolStats) ToolsPerTurn() float64 {
	if t.Turns == 0 {
		return 0
	}
	return float64(t.TotalCalls) / float64(t.Turns)
}

// ToolStats computes the scope's tool volume, reliability, and mix for the Insights page. The
// deduped tool-call numerator and the turn denominator are separate reads, so it wraps them in one
// repeatable-read, read-only snapshot: a concurrent projection update between them could otherwise
// pair TotalCalls from one cohort with Turns from another, making ToolsPerTurn a mixed-snapshot
// figure. Insights threads its own snapshot through toolStatsFrom; this is the standalone
// equivalent. The snapshot takes no row locks, so it never blocks ingest.
func (s *Store) ToolStats(ctx context.Context, f AnalyticsFilter) (ToolStats, error) {
	var out ToolStats
	err := pgx.BeginTxFunc(ctx, s.Pool,
		pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			var err error
			out, err = s.toolStatsFrom(ctx, tx, f)
			return err
		})
	if err != nil {
		return ToolStats{}, fmt.Errorf("tool stats snapshot: %w", err)
	}
	return out, nil
}

// toolStatsFrom computes the scope's tool volume, reliability, and mix, over the
// session_tool_rollup rows of started_at-windowed sessions. The rollup already collapsed
// replayed calls at write time (deriveSessionRollupsTx runs the same dedup the per-session
// signals apply), so the cohort read is a plain sum: per tool across the summed categories,
// which reproduces the old per-tool grouping exactly.
//
// TotalCalls and TotalFailures are summed from the rollup, NOT from the per-session
// session_signals.tool_calls/tool_failures that gatherSignalFacts already stores. That is
// deliberate, not a duplicated derivation. session_signals rows exist only for settled,
// graded sessions (the settle pass writes them once a session idles past the abandoned
// threshold, and a pre-reparse or stale-facts session is left ungraded on purpose), so
// summing them would silently drop every live, unsettled, or ungraded session from the
// fleet tool picture, which the Insights panel must show. The two projections are still the
// same arithmetic: the rollup derivation and gatherSignalFacts share the dedup key, and
// both count a failure as result_status = 'error', so over any fully-graded cohort
// sum(session_signals.tool_calls) == TotalCalls and sum(tool_failures) == TotalFailures
// exactly. TestToolStatsReconcilesWithSessionSignals pins that equality on a settled
// cohort, so the shared dedup shape cannot drift between the two paths without failing a
// test.
//
// The panel shows only the busiest maxToolBars, so the cap belongs in SQL: the query LIMITs to
// that many rows and Postgres bounded-sorts just the top slice rather than ordering and streaming
// every distinct tool (MCP and agent tool names accumulate across fleet history) for the loop to
// discard all but the first few. The fleet totals must still sum over every tool so the headline
// error rate is the true fleet rate even when the bar list is short, and a window count would count
// only the returned rows under a LIMIT, so total_calls, total_failures, and distinct_tools come from
// scalar subqueries over agg. agg is referenced several times, which makes Postgres materialize it
// once, so the grouping runs a single time.
func (s *Store) toolStatsFrom(ctx context.Context, q querier, f AnalyticsFilter) (ToolStats, error) {
	var ts ToolStats

	filter, args := f.clauseFor("s.started_at")
	limitArg := fmt.Sprintf("$%d", len(args)+1)
	args = append(args, maxToolBars)
	rows, err := q.Query(ctx,
		`WITH agg AS (
		   SELECT tr.tool_name,
		          sum(tr.calls)::bigint    AS calls,
		          sum(tr.failures)::bigint AS failures
		     FROM session_tool_rollup tr
		     JOIN sessions s ON s.id = tr.session_id
		    WHERE TRUE`+filter+`
		    GROUP BY tr.tool_name
		 )
		 SELECT tool_name, calls, failures,
		        (SELECT coalesce(sum(calls), 0)    FROM agg) AS total_calls,
		        (SELECT coalesce(sum(failures), 0) FROM agg) AS total_failures,
		        (SELECT count(*)                   FROM agg) AS distinct_tools
		   FROM agg
		  ORDER BY calls DESC, tool_name
		  LIMIT `+limitArg, args...)
	if err != nil {
		return ToolStats{}, fmt.Errorf("query tool stats: %w", err)
	}
	defer rows.Close()

	var distinctTools int
	for rows.Next() {
		var t ToolStat
		if err := rows.Scan(&t.Name, &t.Calls, &t.Failures, &ts.TotalCalls, &ts.TotalFailures, &distinctTools); err != nil {
			return ToolStats{}, fmt.Errorf("scan tool stats: %w", err)
		}
		ts.Tools = append(ts.Tools, t)
	}
	if err := rows.Err(); err != nil {
		return ToolStats{}, fmt.Errorf("iterate tool stats: %w", err)
	}
	if distinctTools > maxToolBars {
		ts.Clipped = distinctTools - maxToolBars
	}

	// Turns is the prompt count the tools-per-turn rate divides by: every human prompt in
	// the cohort, whether or not its session ran a tool. user_message_count counts real
	// prompts as stored (a pure tool-result entry is never a message). One asymmetry to
	// name plainly: a resumed transcript replays prior turns under new ordinals, so its
	// prompts are stored (and counted) again, while the numerator dedupes the replayed
	// calls. So the rate is deduped calls per STORED prompt, a fleet approximation rather
	// than an exact per-logical-prompt figure. The skew is bounded by how often sessions
	// resume, which is rare enough to leave the fleet signal intact.
	filter, args = f.clauseFor("s.started_at")
	if err := q.QueryRow(ctx,
		`SELECT coalesce(sum(s.user_message_count), 0) FROM sessions s WHERE TRUE`+filter, args...).Scan(&ts.Turns); err != nil {
		return ToolStats{}, fmt.Errorf("tool turns denominator: %w", err)
	}
	return ts, nil
}
