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

	// An errored root with its own one-off file edit. Its dollars land in total spend but not in
	// abandoned spend (abandoned counts outcome = 'abandoned' only, so an errored session is spend
	// without being abandoned), and its single edit of a distinct file must not count as a hot file.
	errSession := mkSession(grace, "t-err", 1, "errored", "F", "claude-opus-4-8", "cmd/akari-server/main.go")

	// Two measured context peaks, one below the 8k first histogram edge so the underflow fold is
	// exercised, and the histogram total reconciles with the measured-context cohort.
	for sid, peak := range map[int64]int64{root1: 5000, errSession: 50000} {
		if _, err := st.Pool.Exec(ctx,
			`UPDATE session_signals SET peak_context_tokens = $2, context_reset_count = 0 WHERE session_id = $1`,
			sid, peak); err != nil {
			t.Fatalf("set peak context for %d: %v", sid, err)
		}
	}

	// A day-1 edit of the same churn file the three day-2 sessions touched. The file is now hot
	// across the window (four edits) but edited only once in the day-1 bucket, so the per-bucket
	// hot-file series must still count it in day 1: this is the cross-bucket case the window-hot
	// definition fixes, where a per-bucket definition would hide it.
	mkSession(grace, "t-churn1", 1, "completed", "B", "claude-sonnet-5", churn)

	// A session started two days ahead of render time (a machine with a skewed clock, or a
	// backfilled log dated forward). The trend grid stops at the current bucket, so this
	// session's bucket is off the grid and every series drops it (g.index < 0). The page pins
	// its window to that grid on both edges, so the headline distributions must drop it too: a
	// row the charts cannot show must not inflate the totals printed beside them. Without the
	// f.Until bound the quality headline would count seven while the outcome series summed six.
	mkSession(ada, "t-future", -2, "completed", "A", "claude-sonnet-5", "internal/server/web/future.go")

	// An unpriced, token-bearing usage event on an unknown model (day 2). The pricing table
	// cannot price it, so every cost figure in the window becomes a lower bound and the cache
	// savings total becomes partial. Its cost is NULL, so it adds no dollars and the exact totals
	// above are unchanged, and it rides day 2, so the latest measured cache bucket (day 1) keeps
	// its rate. The cache tokens on an unknown model both flag cost incomplete (via the shared
	// costIncompleteExpr) and leave the saving unpriced.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1, 'pi-unpriced-xyz', 0, 0, 500, 0, NULL, now() - make_interval(days => 2), 't1-unpriced')`,
		root1); err != nil {
		t.Fatalf("seed unpriced usage: %v", err)
	}
	// The session's maintained cost_incomplete flag, so the gallery's per-session cost figures
	// carry the same lower-bound marker the canonical rollups do.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE sessions SET cost_incomplete = true WHERE id = $1`, root1); err != nil {
		t.Fatalf("flag session cost incomplete: %v", err)
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

	// Signals: the outcomes are counted across buckets (six sessions have a shape and a
	// current-version signal in window).
	var outcomeTotal int
	for _, n := range tr.Signals.OutcomeTotal {
		outcomeTotal += n
	}
	if outcomeTotal != 6 {
		t.Errorf("signal outcome total = %d, want 6 (five roots + one subagent)", outcomeTotal)
	}

	// The headline quality distribution and the bucketed outcome series read one cohort: the
	// same started_at-windowed, signals-gated sessions, with no relationship filter on either.
	// Once the page bounds its window to the charted grid they must count the same sessions, so
	// the future-started session is absent from both. A mismatch here means a headline query
	// escaped the f.Until bound and counted a row the series dropped off the grid's upper edge.
	if ins.Quality.Sessions != outcomeTotal {
		t.Errorf("quality headline sessions = %d, bucketed outcome total = %d; they must reconcile (a future-started row leaked into the headline but not the series)", ins.Quality.Sessions, outcomeTotal)
	}

	// Economics: spend covers every outcome (six sessions at $1.50), abandoned spend covers only
	// the one outcome = 'abandoned' session, so the errored and completed dollars are excluded.
	if got := tr.Economics.TotalSpend; got < 8.99 || got > 9.01 {
		t.Errorf("economics total spend = %v, want 9.0 (six sessions at $1.50 each)", got)
	}
	if got := tr.Economics.TotalAbandoned; got < 1.49 || got > 1.51 {
		t.Errorf("abandoned spend = %v, want 1.5 (only the outcome='abandoned' session; errored and completed excluded)", got)
	}
	// The three cost bands (completed, abandoned, other) sum to total spend per bucket, so the
	// stacked chart reconciles with the headline. The errored session's dollars land in other,
	// which is why the completed+abandoned bars alone would fall short of the total.
	var bandSum, otherSum float64
	for i := range tr.Economics.CostCompleted {
		bandSum += tr.Economics.CostCompleted[i] + tr.Economics.CostAbandoned[i] + tr.Economics.CostOther[i]
		otherSum += tr.Economics.CostOther[i]
	}
	if bandSum < 8.99 || bandSum > 9.01 {
		t.Errorf("cost bands sum to %v, want 9.0 (completed + abandoned + other must equal total spend)", bandSum)
	}
	if otherSum < 1.49 || otherSum > 1.51 {
		t.Errorf("other-outcome spend = %v, want 1.5 (the errored session, neither completed nor abandoned)", otherSum)
	}
	if tr.Economics.TotalCacheSavings <= 0 {
		t.Errorf("economics cache savings = %v, want > 0 (cache tokens were seeded)", tr.Economics.TotalCacheSavings)
	}
	// Cache hit rate divides cache reads by every prompt-side token (input + cache read + cache
	// write); the seed's 8000 / (4000 + 8000 + 3000) is ~53%. Dropping cache_write would read ~66%.
	if got := tr.Economics.CacheHitRateLatest; got < 52 || got > 55 {
		t.Errorf("cache hit rate = %v, want ~53 (8000/(4000+8000+3000)); a value near 66 means cache_write was dropped from the denominator", got)
	}
	// Incompleteness propagates: a token-bearing unpriced event makes every window cost figure a
	// lower bound, and cached volume on an unpriced model makes the saving partial. The insights
	// projections must carry the same flags the canonical cost and cache surfaces do.
	if !tr.Economics.CostIncomplete {
		t.Error("economics CostIncomplete = false, want true (a token-bearing unpriced event is in window)")
	}
	if !tr.Economics.CacheSavingsIncomplete {
		t.Error("economics CacheSavingsIncomplete = false, want true (cached volume rode an unpriced model)")
	}
	if !tr.Gallery.CostIncomplete {
		t.Error("gallery CostIncomplete = false, want true (a cohort session is flagged cost_incomplete)")
	}
	if !tr.Subagents.CostShareIncomplete {
		t.Error("subagents CostShareIncomplete = false, want true (the cost share divides lower-bound sums)")
	}

	// Gallery summaries are computed over the full cohort in the store (not the capped Rows), so
	// the headline median duration describes every fully-spanned session. Each seeded session
	// spans 20 minutes, so the median is 1200 seconds over all six.
	if tr.Gallery.Total != 6 {
		t.Errorf("gallery total = %d, want 6 fully-spanned sessions", tr.Gallery.Total)
	}
	if got := tr.Gallery.MedianDurationS; got < 1199 || got > 1201 {
		t.Errorf("gallery median duration = %v, want 1200 (each seeded session spans 20 minutes)", got)
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
	// The churn file is hot across the window (four edits: three on day 2, one on day 1) and the
	// two one-off edits (t3's templ, the errored session's main.go) are not. Under the window-hot
	// definition the hot file counts in each bucket it was edited in, so the per-bucket series
	// sums to two (one for day 2, one for day 1), while the distinct-file window total is one. A
	// per-bucket-hot definition would drop the day-1 edit and sum to one, disagreeing with the
	// tree and TotalHotFiles.
	var hotFiles int
	for _, n := range tr.Churn.Files {
		hotFiles += n
	}
	if hotFiles != 2 {
		t.Errorf("churn per-bucket hot-file series sums to %d, want 2 (the cross-bucket hot file counts in both its buckets)", hotFiles)
	}
	if tr.Churn.TotalHotFiles != 1 {
		t.Errorf("churn TotalHotFiles = %d, want 1 (one distinct file edited more than once across the window)", tr.Churn.TotalHotFiles)
	}
	// Re-edits count only hot files, the same set the tree renders, so the headline total equals
	// the tree's edit sum (the tree is well under its cap here). The one-off edits of the templ
	// and main.go must not inflate it: a sum over every edited file would read six, not four.
	var treeEdits int
	for _, node := range tr.Churn.Tree {
		treeEdits += node.Edits
	}
	if tr.Churn.TotalReEdits != 4 {
		t.Errorf("churn TotalReEdits = %d, want 4 (only the hot file's four edits; the two one-off edits are excluded)", tr.Churn.TotalReEdits)
	}
	if tr.Churn.TotalReEdits != treeEdits {
		t.Errorf("churn TotalReEdits %d != tree edit sum %d; the re-edit headline must reconcile with the rendered tree", tr.Churn.TotalReEdits, treeEdits)
	}

	// Context histogram counts both measured peaks, including the sub-8k one folded into the first
	// bin, so its total reconciles with the two sessions given a peak.
	var histTotal int
	for _, b := range tr.Signals.ContextHistogram {
		histTotal += b.Count
	}
	if histTotal != 2 {
		t.Errorf("context histogram total = %d, want 2 (both measured peaks, including the sub-8k one)", histTotal)
	}
	if len(tr.Signals.ContextHistogram) > 0 && tr.Signals.ContextHistogram[0].Count < 1 {
		t.Errorf("sub-8k peak did not fold into the first bin: %+v", tr.Signals.ContextHistogram[0])
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
