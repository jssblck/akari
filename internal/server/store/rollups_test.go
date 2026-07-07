package store_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
	"github.com/jssblck/akari/migrations"
)

// rollupsDelta is the shared fixture for the rollup reconciliation tests: one session's
// projection with every wrinkle the derivations must handle. Two usage events on one (day,
// model) that must fold, a second model on a second day with an unpriced token-bearing
// event, an undated event (NULL day), a replayed tool call that must dedup, a failing
// call, two deduped edits of one file, an untimestamped prompt whose turn must not
// measure, and an idle gap past the active threshold that must not count.
func rollupsDelta() store.ProjectionDelta {
	t0 := time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC)
	cost := func(v float64) *float64 { return &v }
	return store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "fix the bug", Timestamp: t0},
			{Ordinal: 1, Role: "assistant", Content: "looking", Timestamp: t0.Add(5 * time.Second)},
			{Ordinal: 2, Role: "assistant", Content: "reading", HasToolUse: true, Timestamp: t0.Add(65 * time.Second)},
			{Ordinal: 3, Role: "user", Content: "now fix it", Timestamp: t0.Add(400 * time.Second)},
			{Ordinal: 4, Role: "assistant", Content: "editing", HasToolUse: true, Timestamp: t0.Add(410 * time.Second)},
			{Ordinal: 5, Role: "user", Content: "undated prompt"}, // no timestamp: opens a turn that never measures
			{Ordinal: 6, Role: "assistant", Content: "late reply", HasToolUse: true, Timestamp: t0.Add(3700 * time.Second)},
		},
		ToolCalls: []store.ProjToolCall{
			// A replayed Read: same call id, input, and (absent) result, so the dedup keeps one.
			{MessageOrdinal: 2, CallIndex: 0, ToolName: "Read", Category: "read", InputBody: "read-a", CallUID: "r1"},
			{MessageOrdinal: 2, CallIndex: 1, ToolName: "Read", Category: "read", InputBody: "read-a", CallUID: "r1"},
			// Two distinct edits of the same file: churn_path "a.go" with edits 2.
			{MessageOrdinal: 4, CallIndex: 0, ToolName: "Edit", Category: "edit", FilePath: "/home/grace/akari/a.go", InputBody: "edit-1", CallUID: "e1"},
			{MessageOrdinal: 4, CallIndex: 1, ToolName: "Edit", Category: "edit", FilePath: "/home/grace/akari/a.go", InputBody: "edit-2", CallUID: "e2"},
			// A failing call with an empty category, which must normalize to "other".
			{MessageOrdinal: 6, CallIndex: 0, ToolName: "Bash", Category: "", InputBody: "run", CallUID: "b1"},
		},
		ToolResults: []store.ToolResultDelta{
			{CallUID: "e1", Status: "ok"},
			{CallUID: "b1", Status: "error"},
		},
		Usage: []store.ProjUsage{
			{Model: "m1", Input: 100, Output: 10, CacheRead: 50, CacheWrite: 5, CostUSD: cost(0.5), OccurredAt: t0, DedupKey: "u1", SourceOffset: 10},
			{Model: "m1", Input: 200, Output: 20, CostUSD: cost(1.0), OccurredAt: t0.Add(13 * time.Hour), DedupKey: "u2", SourceOffset: 20}, // same UTC day, folds
			{Model: "m2", Input: 10, OccurredAt: t0.Add(24 * time.Hour), DedupKey: "u3", SourceOffset: 30},                                  // no cost, tokens > 0: unpriced
			{Model: "m1", Input: 7, CostUSD: cost(0.1), DedupKey: "u4", SourceOffset: 40},                                                   // undated: NULL day
		},
		Started: t0,
		Ended:   t0.Add(3700 * time.Second),
	}
}

// dumpTable reads a rollup table's rows for one session into deterministic strings, so the
// tests compare full rows (values included) rather than counts.
func dumpTable(t *testing.T, st *store.Store, table string, sid int64) []string {
	t.Helper()
	order := map[string]string{
		"session_usage_daily":     "day NULLS FIRST, model",
		"session_tool_rollup":     "tool_name, category",
		"session_file_churn":      "churn_path",
		"session_turns":           "turn",
		"session_activity_hourly": "day, hour",
	}[table]
	ctx := context.Background()
	// Whole-row text rendering includes timestamptz columns, which render in the
	// connection's timezone; pin it so the expectations are stable.
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin dump tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SET LOCAL TIME ZONE 'UTC'`); err != nil {
		t.Fatalf("set dump timezone: %v", err)
	}
	rows, err := tx.Query(ctx,
		fmt.Sprintf(`SELECT t::text FROM %s t WHERE session_id = $1 ORDER BY %s`, table, order), sid)
	if err != nil {
		t.Fatalf("dump %s: %v", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan %s dump: %v", table, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s dump: %v", table, err)
	}
	return out
}

// rollupTablesUnderTest mirrors store.rollupTables (unexported); a table added there
// without updating this list will fail TestRollupBackfillMatchesDerivations when its
// backfill diverges, and the migration test below counts backfill statements.
var rollupTablesUnderTest = []string{
	"session_usage_daily",
	"session_tool_rollup",
	"session_file_churn",
	"session_turns",
	"session_activity_hourly",
}

// TestRollupsDerivedInRebuild pins each rollup table's content after a rebuild against
// hand-computed expectations over the fixture delta, then rebuilds with a smaller delta
// and confirms the rollups were replaced absolutely rather than accumulated.
func TestRollupsDerivedInRebuild(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	uid := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	sid := seedSession(t, st, uid, pid, "rollups-1")
	rebuildWith(t, st, sid, rollupsDelta())

	// Usage: (2026-06-03, m1) folds two events; (2026-06-04, m2) is unpriced; the undated
	// event lands on a NULL day. Cost sums skip NULL (coalesce to 0 on the m2 row).
	assertRows(t, st, "session_usage_daily", sid, []string{
		`(` + fmt.Sprint(sid) + `,,m1,7,0,0,0,0.1,f)`,
		`(` + fmt.Sprint(sid) + `,2026-06-03,m1,300,30,50,5,1.5,f)`,
		`(` + fmt.Sprint(sid) + `,2026-06-04,m2,10,0,0,0,0,t)`,
	})

	// Tools: the replayed Read collapses to one call; Bash's empty category normalizes to
	// "other" and its error counts; the two edits are distinct calls.
	assertRows(t, st, "session_tool_rollup", sid, []string{
		`(` + fmt.Sprint(sid) + `,Bash,other,1,1)`,
		`(` + fmt.Sprint(sid) + `,Edit,edit,2,0)`,
		`(` + fmt.Sprint(sid) + `,Read,read,1,0)`,
	})

	// Churn: both edits key on the session-relative path (cwd /home/grace/akari).
	assertRows(t, st, "session_file_churn", sid, []string{
		`(` + fmt.Sprint(sid) + `,a.go,2)`,
	})

	// Turns: turn 1 measures 5s, turn 2 measures 10s; turn 3's prompt is undated so it
	// never stores. prompt_at renders in the connection's UTC timezone.
	assertRows(t, st, "session_turns", sid, []string{
		`(` + fmt.Sprint(sid) + `,1,"2026-06-03 10:00:00+00",5)`,
		`(` + fmt.Sprint(sid) + `,2,"2026-06-03 10:06:40+00",10)`,
	})

	// Activity: hour 10 has five timestamped messages, four tool calls (raw rows, not
	// deduped: replays count as stored, matching the throughput reads), and 75 active
	// seconds (gaps 5+60+10; the 335s gap exceeds the threshold). Hour 11 has the late
	// reply and its call, with its 3290s gap discarded.
	assertRows(t, st, "session_activity_hourly", sid, []string{
		`(` + fmt.Sprint(sid) + `,2026-06-03,10,5,4,75)`,
		`(` + fmt.Sprint(sid) + `,2026-06-03,11,1,1,0)`,
	})

	// A rebuild replaces, never accumulates: after re-deriving from a one-message delta,
	// every rollup shrinks to that projection.
	rebuildWith(t, st, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "user", Content: "hi", Timestamp: time.Date(2026, 6, 5, 9, 0, 0, 0, time.UTC)}},
		Started:  time.Date(2026, 6, 5, 9, 0, 0, 0, time.UTC),
		Ended:    time.Date(2026, 6, 5, 9, 1, 0, 0, time.UTC),
	})
	for _, table := range []string{"session_usage_daily", "session_tool_rollup", "session_file_churn", "session_turns"} {
		if got := dumpTable(t, st, table, sid); len(got) != 0 {
			t.Errorf("%s after shrinking rebuild = %v, want empty", table, got)
		}
	}
	assertRows(t, st, "session_activity_hourly", sid, []string{
		`(` + fmt.Sprint(sid) + `,2026-06-05,9,1,0,0)`,
	})
}

func assertRows(t *testing.T, st *store.Store, table string, sid int64, want []string) {
	t.Helper()
	got := dumpTable(t, st, table, sid)
	if len(got) != len(want) {
		t.Errorf("%s rows = %v, want %v", table, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s row %d = %s, want %s", table, i, got[i], want[i])
		}
	}
}

// TestRollupBackfillMatchesDerivations pins the 0048 migration's corpus-wide backfill
// equal to the per-session derivations rebuildTx runs: it seeds sessions through real
// rebuilds, snapshots the rollups the derivations produced, truncates them, replays the
// migration file's backfill section (everything after its "-- backfill" marker), and
// requires byte-identical rows. This is what lets the same logic live in two dialects
// (a Go constant and a .sql file) without silent drift.
func TestRollupBackfillMatchesDerivations(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	uid := seedUser(t, st, "ada")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	// Two sessions off the fixture delta (the second shifted a day and stripped to a
	// subset) so the backfill exercises cross-session partitioning, not just one session.
	s1 := seedSession(t, st, uid, pid, "backfill-1")
	rebuildWith(t, st, s1, rollupsDelta())
	d2 := rollupsDelta()
	d2.Messages = d2.Messages[:5]
	d2.ToolCalls = d2.ToolCalls[:2]
	d2.ToolResults = nil
	d2.Usage = d2.Usage[:2]
	for i := range d2.Messages {
		if !d2.Messages[i].Timestamp.IsZero() {
			d2.Messages[i].Timestamp = d2.Messages[i].Timestamp.Add(24 * time.Hour)
		}
	}
	for i := range d2.Usage {
		if !d2.Usage[i].OccurredAt.IsZero() {
			d2.Usage[i].OccurredAt = d2.Usage[i].OccurredAt.Add(24 * time.Hour)
		}
	}
	s2 := seedSession(t, st, uid, pid, "backfill-2")
	rebuildWith(t, st, s2, d2)

	snapshot := map[string][]string{}
	for _, table := range rollupTablesUnderTest {
		snapshot[table] = append(dumpTable(t, st, table, s1), dumpTable(t, st, table, s2)...)
		if _, err := st.Pool.Exec(ctx, `TRUNCATE `+table); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}

	// Replay the migration's backfill section verbatim.
	src, err := migrations.FS.ReadFile("0048_insights_rollups.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	marker := "\n-- backfill"
	i := strings.Index(string(src), marker)
	if i < 0 {
		t.Fatalf("migration 0048 has no %q marker", marker)
	}
	// Comment lines go before the split: the marker comment itself contains semicolons.
	stmts := strings.Split(stripSQLComments(string(src)[i:]), ";")
	ran := 0
	for _, stmt := range stmts {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := st.Pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("backfill statement failed: %v\n%s", err, stmt)
		}
		ran++
	}
	if ran != len(rollupTablesUnderTest) {
		t.Fatalf("backfill ran %d statements, want one per rollup table (%d)", ran, len(rollupTablesUnderTest))
	}

	for _, table := range rollupTablesUnderTest {
		got := append(dumpTable(t, st, table, s1), dumpTable(t, st, table, s2)...)
		if len(got) != len(snapshot[table]) {
			t.Errorf("%s backfill rows = %v, want %v", table, got, snapshot[table])
			continue
		}
		for i := range got {
			if got[i] != snapshot[table][i] {
				t.Errorf("%s backfill row %d = %s, want %s (per-session derivation)", table, i, got[i], snapshot[table][i])
			}
		}
	}
}

// stripSQLComments drops line comments so a backfill chunk that is only commentary does
// not count as a statement.
func stripSQLComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "--") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// TestAnnounceCwdChangeRederivesFileChurn pins the one rollup write outside the rebuild:
// an announce that changes a session's cwd rewrites tool_calls.file_rel_path in place, and
// session_file_churn (keyed on those paths) must follow in the same transaction, because a
// cwd-only announce triggers no rebuild.
func TestAnnounceCwdChangeRederivesFileChurn(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	uid := seedUser(t, st, "anna")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	sid := seedSession(t, st, uid, pid, "cwd-change")
	rebuildWith(t, st, sid, rollupsDelta())
	assertRows(t, st, "session_file_churn", sid, []string{
		`(` + fmt.Sprint(sid) + `,a.go,2)`,
	})

	// Re-announce from a different checkout: the edited file no longer sits under the new
	// cwd, so its rel path recomputes to NULL and the churn key falls back to the absolute
	// path. The rollup must re-key without any rebuild.
	if _, err := st.Announce(ctx, store.AnnounceParams{
		UserID: uid, Agent: "claude", SourceSessionID: "cwd-change",
		ProjectID: pid, GitBranch: "main", Cwd: "/home/grace/elsewhere", Machine: "laptop",
	}); err != nil {
		t.Fatalf("re-announce with new cwd: %v", err)
	}
	assertRows(t, st, "session_file_churn", sid, []string{
		`(` + fmt.Sprint(sid) + `,/home/grace/akari/a.go,2)`,
	})
}
