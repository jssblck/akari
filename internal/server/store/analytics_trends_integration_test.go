package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestInsightsTrends exercises the whole trend grid end to end against Postgres: every
// per-bucket query (fleet mix, gallery, velocity, tools, churn, signals, economics,
// subagents, rhythm) runs over one seeded cohort. Its first job is to catch SQL errors that
// live in string literals and so escape the Go compiler; its second is to confirm the grid
// is populated and internally sane (a bucket carries the sessions seeded into it, the
// delegation is detected, the twice-edited file surfaces in the churn tree). The exact
// per-figure math is pinned by the distribution tests and the serializer test; this proves
// the trend layer reconciles with them and does not throw.
func TestInsightsTrends(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	ada := seedUser(t, st, "ada")
	grace := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	// Two active days inside a 7-day window, at a fixed hour so the rhythm grid has a
	// definite cell. daysAgo drives both the session span and its usage event's occurred_at.
	day := func(daysAgo, hour int) time.Time {
		d := time.Now().Add(time.Duration(-daysAgo) * 24 * time.Hour).UTC()
		return time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, time.UTC)
	}

	// mkSession seeds one fully shaped session: a timed turn (prompt + reply) and an edit of
	// churnPath, a signal (outcome/grade), a session span, and a priced usage event with
	// cache tokens on the same day, so every trend query has a row to read.
	mkSession := func(user int64, src string, daysAgo int, outcome, grade, model, churnPath string) int64 {
		sid := seedSession(t, st, user, pid, src)
		start := day(daysAgo, 14)
		if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
			Messages: []store.MessageDelta{
				{Ordinal: 0, Role: "user", Content: "go", Timestamp: start},
				{Ordinal: 1, Role: "assistant", Content: "on it", HasToolUse: true, Timestamp: start.Add(12 * time.Second)},
				{Ordinal: 2, Role: "user", Content: "next", Timestamp: start.Add(60 * time.Second)},
				{Ordinal: 3, Role: "assistant", Content: "done", HasToolUse: true, Timestamp: start.Add(90 * time.Second)},
			},
			ToolCalls: []store.ProjToolCall{
				{MessageOrdinal: 1, CallIndex: 0, ToolName: "Read", Category: "read", CallUID: src + "-r"},
				{MessageOrdinal: 3, CallIndex: 0, ToolName: "Edit", Category: "edit", FilePath: churnPath, CallUID: src + "-e"},
			},
		}); err != nil {
			t.Fatalf("apply delta %s: %v", src, err)
		}
		setSessionShape(t, st, ctx, sid, start, start.Add(20*time.Minute), 4, 2)
		insertSignal(t, st, ctx, sid, quality.Version, outcome, grade)
		seedUsageCache(t, st, sid, model, 1.5, 4000, 2000, 8000, 3000, daysAgo, src+"-u")
		return sid
	}

	// Two models across two days so the fleet mix has more than one band, and the same file
	// edited by two sessions so it clears the churn tree's "more than once" bar.
	churn := "internal/server/store/analytics.go"
	root1 := mkSession(ada, "t1", 2, "completed", "A", "claude-sonnet-5", churn)
	mkSession(ada, "t2", 2, "abandoned", "C", "claude-sonnet-5", churn)
	mkSession(grace, "t3", 1, "completed", "B", "claude-opus-4-8", "internal/server/web/insights.templ")

	// A subagent child of root1 on the same day, so the delegation trend, the fan-out spread,
	// the cost share, and the deepest-tree figure all have something to read.
	child := mkSession(ada, "t1-sub", 2, "completed", "A", "claude-sonnet-5", churn)
	if _, err := st.Pool.Exec(ctx,
		`UPDATE sessions SET parent_session_id = $1, relationship_type = 'subagent' WHERE id = $2`,
		root1, child); err != nil {
		t.Fatalf("link subagent: %v", err)
	}

	since := time.Now().Add(-7 * 24 * time.Hour)
	ins, err := st.Insights(ctx, store.AnalyticsFilter{Since: since, Bucket: "day"})
	if err != nil {
		t.Fatalf("insights with trends: %v", err)
	}

	tr := ins.Trends
	if tr == nil || !tr.HasData() {
		t.Fatalf("Trends missing or empty: %+v", tr)
	}
	if tr.Unit != "day" {
		t.Errorf("Trends.Unit = %q, want day", tr.Unit)
	}
	if len(tr.BucketStarts) < 2 {
		t.Errorf("grid has %d buckets, want at least the two active days", len(tr.BucketStarts))
	}

	// Fleet mix: both models seeded appear as their own bands (only two, so no fold).
	if !tr.FleetMix.HasData() || len(tr.FleetMix.Models) < 2 {
		t.Errorf("fleet mix = %+v, want at least two model bands", tr.FleetMix.Models)
	}

	// Signals: the outcomes are counted across buckets (four sessions have a shape and a
	// current-version signal in window).
	var outcomeTotal int
	for _, n := range tr.Signals.OutcomeTotal {
		outcomeTotal += n
	}
	if outcomeTotal != 4 {
		t.Errorf("signal outcome total = %d, want 4 (three roots + one subagent)", outcomeTotal)
	}

	// Economics: real spend and a positive cache saving from the seeded cache tokens.
	if tr.Economics.TotalSpend <= 0 {
		t.Errorf("economics total spend = %v, want > 0", tr.Economics.TotalSpend)
	}
	if tr.Economics.TotalCacheSavings <= 0 {
		t.Errorf("economics cache savings = %v, want > 0 (cache tokens were seeded)", tr.Economics.TotalCacheSavings)
	}

	// Tools: the reliability scatter carries the seeded tools, and the churn tree carries the
	// file two sessions edited.
	if len(tr.Tools.Reliability) == 0 {
		t.Error("tool reliability is empty, want the seeded Read/Edit tools")
	}
	var foundChurn bool
	for _, node := range tr.Churn.Tree {
		if node.Path == churn {
			foundChurn = true
			if node.Edits < 2 {
				t.Errorf("churn node %q edits = %d, want at least 2", churn, node.Edits)
			}
		}
	}
	if !foundChurn {
		t.Errorf("churn tree missing the twice-edited file %q: %+v", churn, tr.Churn.Tree)
	}

	// Subagents: the one child is detected, at least one root delegates, and the tree is two
	// deep (root -> subagent).
	if tr.Subagents.SubagentSessionsInWindow < 1 {
		t.Errorf("subagent sessions in window = %d, want at least 1", tr.Subagents.SubagentSessionsInWindow)
	}
	if tr.Subagents.SessionsThatDelegatePct <= 0 {
		t.Errorf("sessions-that-delegate share = %v, want > 0", tr.Subagents.SessionsThatDelegatePct)
	}
	if tr.Subagents.DeepestTree < 2 {
		t.Errorf("deepest tree = %d, want at least 2 (root -> subagent)", tr.Subagents.DeepestTree)
	}

	// Rhythm: the seeded 14:00 activity lands somewhere in the grid.
	if !tr.Rhythm.HasData() {
		t.Error("rhythm grid is empty, want the seeded afternoon activity")
	}

	// The distributions-only path (no bucket) still leaves Trends nil, so a caller that does
	// not want the grid pays nothing for it.
	plain, err := st.Insights(ctx, store.AnalyticsFilter{Since: since})
	if err != nil {
		t.Fatalf("insights without trends: %v", err)
	}
	if plain.Trends != nil {
		t.Error("Trends should be nil when the filter names no bucket")
	}
}
