package store

import (
	"context"
	"fmt"
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

// ToolStats computes the scope's tool volume, reliability, and mix on its own pooled
// connection for the Insights page. The snapshot path threads toolStatsFrom so every panel
// reads one MVCC snapshot.
func (s *Store) ToolStats(ctx context.Context, f AnalyticsFilter) (ToolStats, error) {
	return s.toolStatsFrom(ctx, s.Pool, f)
}

// toolStatsFrom computes the scope's tool volume, reliability, and mix. The per-tool query
// dedupes replayed calls exactly as the per-session signals do (a resumed or compacted
// transcript replays prior turns verbatim, and a call's id legitimately rides several
// rows), but partitions the dedup by session_id as well, since a call id is only unique
// within its session. The mix is clipped to the busiest tools for legibility while the
// fleet totals sum over every tool, so the headline error rate is the true fleet rate
// even when the bar list is short.
func (s *Store) toolStatsFrom(ctx context.Context, q querier, f AnalyticsFilter) (ToolStats, error) {
	var ts ToolStats

	filter, args := f.clauseFor("s.started_at")
	rows, err := q.Query(ctx,
		`WITH scoped AS (
		   SELECT tc.session_id, tc.message_ordinal, tc.call_index, tc.tool_name,
		          tc.input_sha256, tc.result_status, tc.call_uid
		     FROM tool_calls tc
		     JOIN sessions s ON s.id = tc.session_id
		    WHERE TRUE`+filter+`
		 ),
		 ranked AS (
		   SELECT tool_name, result_status,
		          row_number() OVER (
		            PARTITION BY `+dedupToolCallsPartition+`
		            ORDER BY message_ordinal, call_index
		          ) AS rn
		     FROM scoped
		 ),
		 agg AS (
		   SELECT tool_name,
		          count(*) AS calls,
		          count(*) FILTER (WHERE result_status = 'error') AS failures
		     FROM ranked WHERE rn = 1
		    GROUP BY tool_name
		 )
		 SELECT tool_name, calls, failures,
		        sum(calls) OVER ()    AS total_calls,
		        sum(failures) OVER () AS total_failures,
		        count(*) OVER ()      AS distinct_tools
		   FROM agg
		  ORDER BY calls DESC, tool_name`, args...)
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
		if len(ts.Tools) < maxToolBars {
			ts.Tools = append(ts.Tools, t)
		}
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
