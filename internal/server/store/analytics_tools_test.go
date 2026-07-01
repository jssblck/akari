package store_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// toolByName finds a tool's stat in the mix, failing the test when it is absent so a
// missing tool reads as a clear failure rather than a zero value.
func toolByName(t *testing.T, tools []store.ToolStat, name string) store.ToolStat {
	t.Helper()
	for _, x := range tools {
		if x.Name == name {
			return x
		}
	}
	t.Fatalf("tool %q not found in mix %+v", name, tools)
	return store.ToolStat{}
}

// TestToolStats pins the fleet tool figures against a known set of calls: the mix and its
// ordering, the deduped counts (a replayed call collapses), the per-tool error rates, and
// the tools-per-turn denominator. It also confirms the window and per-user scoping narrow
// the same way the rest of the Insights page does.
func TestToolStats(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	ada := seedUser(t, st, "ada")
	grace := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	recent := time.Now().Add(-1 * time.Hour)
	old := time.Now().Add(-400 * 24 * time.Hour)

	// Ada's session: three distinct Reads (one replayed across two later turns, which must
	// collapse), two Edits (one failing), two Bash calls (both failing). Deduped that is 7
	// calls and 3 failures; two prompts.
	s1 := seedSession(t, st, ada, pid, "t1")
	if err := st.ApplyProjectionDelta(ctx, s1, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "go"},
			{Ordinal: 1, Role: "assistant", Content: "work", HasToolUse: true},
			{Ordinal: 2, Role: "assistant", Content: "replay one", HasToolUse: true},
			{Ordinal: 3, Role: "assistant", Content: "replay two", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Read", Category: "read", InputBody: "read-a", CallUID: "r1"},
			{MessageOrdinal: 1, CallIndex: 1, ToolName: "Read", Category: "read", InputBody: "read-b", CallUID: "r2"},
			{MessageOrdinal: 1, CallIndex: 2, ToolName: "Read", Category: "read", InputBody: "read-c", CallUID: "r3"},
			{MessageOrdinal: 1, CallIndex: 3, ToolName: "Edit", Category: "edit", FilePath: "a.go", InputBody: "edit-a", CallUID: "e1"},
			{MessageOrdinal: 1, CallIndex: 4, ToolName: "Edit", Category: "edit", FilePath: "b.go", InputBody: "edit-b", CallUID: "e2"},
			{MessageOrdinal: 1, CallIndex: 5, ToolName: "Bash", Category: "bash", InputBody: "bash-a", CallUID: "b1"},
			{MessageOrdinal: 1, CallIndex: 6, ToolName: "Bash", Category: "bash", InputBody: "bash-b", CallUID: "b2"},
			// The r1 Read replayed verbatim across two later turns: same id, tool, input, and
			// (after back-patch) result, so all three collapse to one call.
			{MessageOrdinal: 2, CallIndex: 0, ToolName: "Read", Category: "read", InputBody: "read-a", CallUID: "r1"},
			{MessageOrdinal: 3, CallIndex: 0, ToolName: "Read", Category: "read", InputBody: "read-a", CallUID: "r1"},
		},
		ToolResults: []store.ToolResultDelta{
			{CallUID: "r1", Status: "ok"}, {CallUID: "r2", Status: "ok"}, {CallUID: "r3", Status: "ok"},
			{CallUID: "e1", Status: "ok"}, {CallUID: "e2", Status: "error"},
			{CallUID: "b1", Status: "error"}, {CallUID: "b2", Status: "error"},
		},
	}); err != nil {
		t.Fatalf("apply s1: %v", err)
	}
	setSessionShape(t, st, ctx, s1, recent, recent.Add(10*time.Minute), 6, 2)

	// Grace's session: a Read and a Grep, started long ago so a window drops it and a
	// per-user scope excludes it. One prompt.
	s2 := seedSession(t, st, grace, pid, "t2")
	if err := st.ApplyProjectionDelta(ctx, s2, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "hi"},
			{Ordinal: 1, Role: "assistant", Content: "sure", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Read", Category: "read", InputBody: "g-read", CallUID: "gr"},
			{MessageOrdinal: 1, CallIndex: 1, ToolName: "Grep", Category: "search", InputBody: "g-grep", CallUID: "gg"},
		},
		ToolResults: []store.ToolResultDelta{
			{CallUID: "gr", Status: "ok"}, {CallUID: "gg", Status: "ok"},
		},
	}); err != nil {
		t.Fatalf("apply s2: %v", err)
	}
	setSessionShape(t, st, ctx, s2, old, old.Add(5*time.Minute), 2, 1)

	// Ada only: 7 deduped calls, 3 failures, 2 prompts.
	ada1, err := st.ToolStats(ctx, store.AnalyticsFilter{Username: "ada"})
	if err != nil {
		t.Fatalf("tool stats (ada): %v", err)
	}
	if ada1.TotalCalls != 7 || ada1.TotalFailures != 3 || ada1.Turns != 2 {
		t.Errorf("ada totals = {calls %d, fail %d, turns %d}, want {7, 3, 2}", ada1.TotalCalls, ada1.TotalFailures, ada1.Turns)
	}
	if math.Abs(ada1.ErrorRate()-3.0/7.0) > 0.0001 {
		t.Errorf("ada error rate = %.4f, want %.4f", ada1.ErrorRate(), 3.0/7.0)
	}
	if math.Abs(ada1.ToolsPerTurn()-3.5) > 0.0001 {
		t.Errorf("ada tools/turn = %.4f, want 3.5", ada1.ToolsPerTurn())
	}
	// Mix ordering: Read (3) leads, then Bash and Edit tie at 2 and order by name (Bash
	// before Edit).
	if len(ada1.Tools) != 3 || ada1.Tools[0].Name != "Read" || ada1.Tools[1].Name != "Bash" || ada1.Tools[2].Name != "Edit" {
		t.Errorf("ada mix order = %+v, want Read, Bash, Edit", ada1.Tools)
	}
	if r := toolByName(t, ada1.Tools, "Read"); r.Calls != 3 || r.Failures != 0 {
		t.Errorf("Read = {calls %d, fail %d}, want {3, 0} (the replay collapsed)", r.Calls, r.Failures)
	}
	if b := toolByName(t, ada1.Tools, "Bash"); b.Calls != 2 || b.Failures != 2 || math.Abs(b.ErrorRate()-1.0) > 0.0001 {
		t.Errorf("Bash = {calls %d, fail %d, rate %.2f}, want {2, 2, 1.00}", b.Calls, b.Failures, b.ErrorRate())
	}
	if e := toolByName(t, ada1.Tools, "Edit"); e.Calls != 2 || e.Failures != 1 || math.Abs(e.ErrorRate()-0.5) > 0.0001 {
		t.Errorf("Edit = {calls %d, fail %d, rate %.2f}, want {2, 1, 0.50}", e.Calls, e.Failures, e.ErrorRate())
	}

	// Unscoped over all time: Grace's Read and Grep join, so 9 calls across four tools.
	all, err := st.ToolStats(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("tool stats (all): %v", err)
	}
	if all.TotalCalls != 9 || all.TotalFailures != 3 || all.Turns != 3 {
		t.Errorf("all totals = {calls %d, fail %d, turns %d}, want {9, 3, 3}", all.TotalCalls, all.TotalFailures, all.Turns)
	}
	if r := toolByName(t, all.Tools, "Read"); r.Calls != 4 {
		t.Errorf("all Read calls = %d, want 4 (Ada's 3 plus Grace's 1)", r.Calls)
	}
	toolByName(t, all.Tools, "Grep") // present only in the unscoped mix

	// A trailing window keyed on started_at drops Grace's old session, back to Ada's seven.
	windowed, err := st.ToolStats(ctx, store.AnalyticsFilter{Since: time.Now().Add(-90 * 24 * time.Hour)})
	if err != nil {
		t.Fatalf("tool stats (windowed): %v", err)
	}
	if windowed.TotalCalls != 7 || windowed.Turns != 2 {
		t.Errorf("windowed totals = {calls %d, turns %d}, want {7, 2}", windowed.TotalCalls, windowed.Turns)
	}
}

// TestToolStatsCrossSessionNoCollapse guards the cohort dedup partition: an identical call
// (same id, tool, input, result) in two DIFFERENT sessions must count twice, since a call
// id is only unique within its session. The per-session dedup adds session_id to the
// partition precisely so a shared id across sessions does not collapse.
func TestToolStatsCrossSessionNoCollapse(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	ada := seedUser(t, st, "ada")
	grace := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	same := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "go"},
			{Ordinal: 1, Role: "assistant", Content: "work", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Read", Category: "read", InputBody: "same", CallUID: "shared"},
		},
		ToolResults: []store.ToolResultDelta{{CallUID: "shared", Status: "ok"}},
	}
	if err := st.ApplyProjectionDelta(ctx, seedSession(t, st, ada, pid, "x-a"), same); err != nil {
		t.Fatalf("apply session a: %v", err)
	}
	if err := st.ApplyProjectionDelta(ctx, seedSession(t, st, grace, pid, "x-b"), same); err != nil {
		t.Fatalf("apply session b: %v", err)
	}

	ts, err := st.ToolStats(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("tool stats: %v", err)
	}
	if ts.TotalCalls != 2 {
		t.Errorf("TotalCalls = %d, want 2 (a shared id across sessions must not collapse)", ts.TotalCalls)
	}
	if r := toolByName(t, ts.Tools, "Read"); r.Calls != 2 {
		t.Errorf("Read calls = %d, want 2", r.Calls)
	}
}

// TestToolStatsNullUidNamespace mirrors the per-session null-id guard at cohort scope: two
// id-less calls must never group with each other, and a real id that resembles the
// synthetic "ordinal:index" key must not collide with it. All three count.
func TestToolStatsNullUidNamespace(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u := seedUser(t, st, "ada")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	sid := seedSession(t, st, u, pid, "ns")
	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "go"},
			{Ordinal: 1, Role: "assistant", Content: "work", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Read", Category: "read", CallUID: ""},    // no id
			{MessageOrdinal: 1, CallIndex: 1, ToolName: "Read", Category: "read", CallUID: ""},    // no id, otherwise identical
			{MessageOrdinal: 1, CallIndex: 2, ToolName: "Read", Category: "read", CallUID: "1:0"}, // real id resembling the synthetic key
		},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	ts, err := st.ToolStats(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("tool stats: %v", err)
	}
	if ts.TotalCalls != 3 {
		t.Errorf("TotalCalls = %d, want 3 (id-less calls never collapse, real id must not collide)", ts.TotalCalls)
	}
}

// TestToolStatsNoTools confirms a cohort with prompts but no tool calls reports no tool
// data (rather than erroring) while still counting the prompts for the denominator.
func TestToolStatsNoTools(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u := seedUser(t, st, "ada")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	sid := seedSession(t, st, u, pid, "chat-only")
	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "explain this"},
			{Ordinal: 1, Role: "assistant", Content: "here is the explanation"},
		},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	setSessionShape(t, st, ctx, sid, time.Now().Add(-time.Hour), time.Now().Add(-50*time.Minute), 2, 3)

	ts, err := st.ToolStats(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("tool stats: %v", err)
	}
	if ts.HasData() || ts.TotalCalls != 0 || ts.TotalFailures != 0 {
		t.Errorf("no-tool cohort = {hasData %v, calls %d, fail %d}, want {false, 0, 0}", ts.HasData(), ts.TotalCalls, ts.TotalFailures)
	}
	if ts.Turns != 3 {
		t.Errorf("Turns = %d, want 3 (prompts still counted)", ts.Turns)
	}
	if ts.ErrorRate() != 0 || ts.ToolsPerTurn() != 0 {
		t.Errorf("empty rates = {err %.2f, perTurn %.2f}, want 0/0", ts.ErrorRate(), ts.ToolsPerTurn())
	}
}

// TestToolStatsClips confirms the mix is capped at maxToolBars with the overflow reported
// in Clipped, while the fleet totals still sum over every tool.
func TestToolStatsClips(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u := seedUser(t, st, "ada")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	const distinct = 13
	calls := make([]store.ProjToolCall, 0, distinct)
	results := make([]store.ToolResultDelta, 0, distinct)
	for i := 0; i < distinct; i++ {
		uid := fmt.Sprintf("c%d", i)
		calls = append(calls, store.ProjToolCall{
			MessageOrdinal: 1, CallIndex: i, ToolName: fmt.Sprintf("Tool%02d", i),
			Category: "read", InputBody: uid, CallUID: uid,
		})
		results = append(results, store.ToolResultDelta{CallUID: uid, Status: "ok"})
	}
	sid := seedSession(t, st, u, pid, "clip")
	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "go"},
			{Ordinal: 1, Role: "assistant", Content: "work", HasToolUse: true},
		},
		ToolCalls:   calls,
		ToolResults: results,
	}); err != nil {
		t.Fatalf("apply clip session: %v", err)
	}

	ts, err := st.ToolStats(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("tool stats: %v", err)
	}
	if ts.TotalCalls != distinct {
		t.Errorf("TotalCalls = %d, want %d (totals sum over every tool)", ts.TotalCalls, distinct)
	}
	if len(ts.Tools) != 10 {
		t.Errorf("shown tools = %d, want 10 (the mix is capped)", len(ts.Tools))
	}
	if ts.Clipped != distinct-10 {
		t.Errorf("Clipped = %d, want %d", ts.Clipped, distinct-10)
	}
}
