package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The insights rollup tables (migration 0048) are per-session pre-aggregations of the
// projection, kept exactly as fresh as the projection itself: rebuildTx re-derives them
// inside the same transaction that rewrites messages, tool_calls, and usage_events, so
// there is no refresh cadence and no staleness window. Each table is defined by one SQL
// derivation over the projection rows, held here and run per session; the 0048 migration's
// backfill runs the same statements corpus-wide (spelled out again in the .sql file, with
// the per-session and backfill forms pinned equal by TestRollupBackfillMatchesDerivations).
//
// The grain rule: every rollup keys on session_id and carries no dimension sessions or
// session_signals already carry. Project, user, agent, machine, grade, and outcome join in
// at read time, so a session getting graded or re-graded never touches a rollup row, and
// the tables stay out of the settle pass entirely. The one write outside the rebuild is the
// announce path's cwd change, which rewrites tool_calls.file_rel_path in place and so must
// re-derive session_file_churn in the same transaction (see ingest.go).
//
// docs/data-aggregation.md carries the base/invariant table these join; changing any
// derivation here is a rebuild-derived-output change and takes a parse.Epoch bump, exactly
// like a reducer or scoring change (the reparse it forces is what re-derives the corpus).

// sessionUsageDailyDerive folds one session's usage_events into per-(UTC day, model) sums:
// the four token classes, the summed cost, and whether any folded event was token-bearing
// but unpriced (the costIncompleteExpr base, so read-side bool_or(unpriced) reproduces it).
// An undated event folds into a NULL day, preserving the documented rollup-versus-analytics
// gap (docs/data-aggregation.md): the dated consumers filter day IS NOT NULL, and the
// ledger reconciliation counts the NULL-day rows as exactly the undated usage.
const sessionUsageDailyDerive = `
INSERT INTO session_usage_daily
  (session_id, day, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd, unpriced)
SELECT ue.session_id,
       (ue.occurred_at AT TIME ZONE 'UTC')::date,
       ue.model,
       coalesce(sum(ue.input_tokens), 0),
       coalesce(sum(ue.output_tokens), 0),
       coalesce(sum(ue.cache_read_tokens), 0),
       coalesce(sum(ue.cache_write_tokens), 0),
       coalesce(sum(ue.cost_usd), 0),
       coalesce(bool_or(ue.cost_usd IS NULL AND (ue.input_tokens + ue.output_tokens + ue.cache_read_tokens + ue.cache_write_tokens + ue.reasoning_tokens) > 0), false)
  FROM usage_events ue
 WHERE ue.session_id = $1
 GROUP BY 1, 2, 3`

// sessionToolRollupDerive collapses one session's replayed tool-call rows once, at write
// time, with the same partition every cohort query used to run per read
// (dedupToolCallsPartition), then counts calls and failures per (tool, category). The
// category is normalized exactly as the reads normalize it (empty folds to "other"), and
// rides the rn=1 row the dedup keeps, so a replay that shifted category cannot double a
// call.
const sessionToolRollupDerive = `
INSERT INTO session_tool_rollup (session_id, tool_name, category, calls, failures)
SELECT $1, tool_name, cat,
       count(*),
       count(*) FILTER (WHERE result_status = 'error')
  FROM (
    SELECT tool_name,
           coalesce(NULLIF(category, ''), 'other') AS cat,
           result_status,
           row_number() OVER (
             PARTITION BY ` + dedupToolCallsPartition + `
             ORDER BY message_ordinal, call_index
           ) AS rn
      FROM tool_calls
     WHERE session_id = $1
  ) ranked
 WHERE rn = 1
 GROUP BY tool_name, cat`

// sessionFileChurnDerive counts one session's deduped edit calls per churn path: the
// worktree-invariant file_rel_path, coalesced onto the absolute file_path when no relative
// form exists, over edit-category calls that carry a parsed path. These are the exact
// predicates and key the churn queries used per read; the "hot" (edited more than once)
// cut stays at read time, because hotness is a window property across sessions, not a
// per-session fact.
const sessionFileChurnDerive = `
INSERT INTO session_file_churn (session_id, churn_path, edits)
SELECT $1, churn_path, count(*)
  FROM (
    SELECT COALESCE(file_rel_path, file_path) AS churn_path,
           row_number() OVER (
             PARTITION BY ` + dedupToolCallsPartition + `
             ORDER BY message_ordinal, call_index
           ) AS rn
      FROM tool_calls
     WHERE session_id = $1 AND category = 'edit' AND file_path IS NOT NULL
  ) ranked
 WHERE rn = 1
 GROUP BY churn_path`

// sessionTurnsDerive labels one session's prompt-to-reply cycles once, at write time, with
// the turn fold the velocity reads used to run per query: a running count of user-role
// messages over EVERY message (an undated prompt still opens its turn, so a later reply
// cannot misattribute to the prior prompt), the prompt instant as the turn's earliest
// user timestamp, and the reply instant as the first timestamped assistant message by
// ordinal. Only measured turns are stored (both instants present, reply not before
// prompt): they are the rows every percentile and count read, and an unmeasured turn
// contributed nothing to any consumer. response_secs keeps the extract(epoch ...) float
// verbatim so read-side percentiles interpolate the same values the per-read fold did.
const sessionTurnsDerive = `
INSERT INTO session_turns (session_id, turn, prompt_at, response_secs)
WITH m AS (
  SELECT ordinal, role, timestamp,
         count(*) FILTER (WHERE role = 'user')
           OVER (ORDER BY ordinal ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS turn
    FROM messages
   WHERE session_id = $1
),
user_turns AS (
  SELECT turn, min(timestamp) FILTER (WHERE role = 'user') AS user_at
    FROM m WHERE turn >= 1 GROUP BY turn
),
asst_turns AS (
  SELECT DISTINCT ON (turn) turn, timestamp AS asst_at
    FROM m WHERE turn >= 1 AND role = 'assistant' AND timestamp IS NOT NULL
   ORDER BY turn, ordinal
)
SELECT $1, u.turn, u.user_at, extract(epoch FROM (a.asst_at - u.user_at))
  FROM user_turns u
  JOIN asst_turns a ON a.turn = u.turn
 WHERE u.user_at IS NOT NULL AND a.asst_at >= u.user_at`

// sessionActivityHourlyDerive buckets one session's timestamped activity by UTC (day,
// hour): message count, tool-call count (a call counts under its owning message's
// instant), and gap-filtered active seconds. A gap is the delta between consecutive
// timestamped messages by ordinal, kept when it is positive and at most activeGapSeconds,
// and attributed to the LATER message's hour, matching the per-read lag() fold it
// replaces. Hours with tool calls are a subset of hours with messages (a counted call's
// owning message is timestamped), so the tool counts left-join onto the message hours.
var sessionActivityHourlyDerive = fmt.Sprintf(`
INSERT INTO session_activity_hourly (session_id, day, hour, messages, tool_calls, active_seconds)
WITH msgs AS (
  SELECT (timestamp AT TIME ZONE 'UTC')::date AS day,
         extract(hour FROM timestamp AT TIME ZONE 'UTC')::smallint AS hour,
         extract(epoch FROM (timestamp - lag(timestamp) OVER (ORDER BY ordinal))) AS gap
    FROM messages
   WHERE session_id = $1 AND timestamp IS NOT NULL
),
mh AS (
  SELECT day, hour, count(*) AS messages,
         coalesce(sum(gap) FILTER (WHERE gap > 0 AND gap <= %d), 0) AS active_seconds
    FROM msgs GROUP BY day, hour
),
th AS (
  SELECT (m.timestamp AT TIME ZONE 'UTC')::date AS day,
         extract(hour FROM m.timestamp AT TIME ZONE 'UTC')::smallint AS hour,
         count(*) AS tool_calls
    FROM tool_calls tc
    JOIN messages m ON m.session_id = tc.session_id AND m.ordinal = tc.message_ordinal
   WHERE tc.session_id = $1 AND m.timestamp IS NOT NULL
   GROUP BY 1, 2
)
SELECT $1, mh.day, mh.hour, mh.messages, coalesce(th.tool_calls, 0), mh.active_seconds
  FROM mh
  LEFT JOIN th ON th.day = mh.day AND th.hour = mh.hour`, activeGapSeconds)

// rollupTables names every insights rollup table, in the order the derivations run. The
// delete loop and the reconciliation tests range over it so adding a table cannot miss one.
var rollupTables = []string{
	"session_usage_daily",
	"session_tool_rollup",
	"session_file_churn",
	"session_turns",
	"session_activity_hourly",
}

// rollupDerivations pairs each rollup table with its per-session derivation, in
// rollupTables order.
var rollupDerivations = []struct {
	table  string
	derive string
}{
	{"session_usage_daily", sessionUsageDailyDerive},
	{"session_tool_rollup", sessionToolRollupDerive},
	{"session_file_churn", sessionFileChurnDerive},
	{"session_turns", sessionTurnsDerive},
	{"session_activity_hourly", sessionActivityHourlyDerive},
}

// deriveSessionRollupsTx rewrites one session's rows in every insights rollup table from
// the projection rows the surrounding transaction just wrote. Delete-then-derive, never
// increment, so the rollups equal a fresh derivation over the projection by construction
// (the same absolute-write discipline the sessions.total_* columns follow). It runs inside
// rebuildTx under the session row lock, so concurrent rebuilds of one session serialize
// and the rollups commit atomically with the projection they summarize.
func deriveSessionRollupsTx(ctx context.Context, tx pgx.Tx, sessionID int64) error {
	for _, t := range rollupTables {
		if _, err := tx.Exec(ctx, `DELETE FROM `+t+` WHERE session_id = $1`, sessionID); err != nil {
			return fmt.Errorf("clear %s for session %d: %w", t, sessionID, err)
		}
	}
	for _, d := range rollupDerivations {
		if _, err := tx.Exec(ctx, d.derive, sessionID); err != nil {
			return fmt.Errorf("derive %s for session %d: %w", d.table, sessionID, err)
		}
	}
	return nil
}

// DeriveSessionRollups rewrites one session's insights rollups from its current
// projection rows, in a transaction of its own. Production code never needs it (rebuildTx
// and the announce path maintain the rollups inline); it exists for tests that seed
// projection rows with direct INSERTs, bypassing the rebuild, and must bring the rollups
// along through the same derivation SQL the write path runs.
func (s *Store) DeriveSessionRollups(ctx context.Context, sessionID int64) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("derive rollups for session %d: begin: %w", sessionID, err)
	}
	defer tx.Rollback(ctx)
	if err := deriveSessionRollupsTx(ctx, tx, sessionID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// deriveSessionFileChurnTx rewrites one session's file-churn rollup alone. The announce
// path calls this when a cwd change rewrites tool_calls.file_rel_path in place (the one
// projection write outside the rebuild): churn_path is derived from that column, so the
// rollup must follow it in the same transaction or it would key hot files under the stale
// anchor until the session's next rebuild, which a cwd-only announce never triggers.
func deriveSessionFileChurnTx(ctx context.Context, tx pgx.Tx, sessionID int64) error {
	if _, err := tx.Exec(ctx, `DELETE FROM session_file_churn WHERE session_id = $1`, sessionID); err != nil {
		return fmt.Errorf("clear session_file_churn for session %d: %w", sessionID, err)
	}
	if _, err := tx.Exec(ctx, sessionFileChurnDerive, sessionID); err != nil {
		return fmt.Errorf("derive session_file_churn for session %d: %w", sessionID, err)
	}
	return nil
}
