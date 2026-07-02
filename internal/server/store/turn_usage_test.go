package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// seedForReads provisions a user, a remote project, and a session (with a known cwd so tool paths
// under it derive a relative form), returning the session id.
func seedForReads(t *testing.T, st *store.Store) int64 {
	t.Helper()
	ctx := context.Background()
	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	return seedSession(t, st, u.ID, projectID, "sess-reads")
}

// TestSessionTurnUsageFolds pins that per-turn usage sums a turn's streamed chunks, computes
// context occupancy as input + cache read + cache write (output excluded), leaves cost NULL only
// when every contributing row is unpriced (a lower-bound partial when some rows are priced), and
// skips rows with no message ordinal.
func TestSessionTurnUsageFolds(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedForReads(t, st)

	ord0, ord1 := 0, 1
	cost := func(f float64) *float64 { return &f }
	// Turn 0: two streamed chunks, both priced. Turn 1: one priced row, one unpriced row (a mixed
	// group, so the cost is a priced partial). A row with no ordinal must not land in any turn.
	delta := store.ProjectionDelta{
		Usage: []store.ProjUsage{
			{MessageOrdinal: &ord0, Model: "gpt-5", Input: 1000, Output: 200, CacheRead: 5000, CacheWrite: 300, Reasoning: 40, CostUSD: cost(0.10), SourceOffset: 1, SourceIndex: 0},
			{MessageOrdinal: &ord0, Model: "gpt-5", Input: 500, Output: 100, CacheRead: 2000, CacheWrite: 0, Reasoning: 10, CostUSD: cost(0.05), SourceOffset: 2, SourceIndex: 0},
			{MessageOrdinal: &ord1, Model: "gpt-5", Input: 800, Output: 400, CacheRead: 100000, CacheWrite: 0, CostUSD: cost(0.30), SourceOffset: 3, SourceIndex: 0},
			{MessageOrdinal: &ord1, Model: "mystery", Input: 100, Output: 50, CacheRead: 0, CacheWrite: 0, CostUSD: nil, SourceOffset: 4, SourceIndex: 0},
			{MessageOrdinal: nil, Model: "gpt-5", Input: 999, Output: 999, CostUSD: cost(9.99), SourceOffset: 5, SourceIndex: 0},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}

	usage, err := st.SessionTurnUsage(ctx, sid)
	if err != nil {
		t.Fatalf("turn usage: %v", err)
	}
	if len(usage) != 2 {
		t.Fatalf("got %d turns, want 2 (the NULL-ordinal row is skipped)", len(usage))
	}

	u0 := usage[0]
	if u0.Input != 1500 || u0.Output != 300 || u0.CacheRead != 7000 || u0.CacheWrite != 300 || u0.Reasoning != 50 {
		t.Errorf("turn 0 tokens = %+v, want summed across both chunks", u0)
	}
	// Context occupancy excludes output: 1500 + 7000 + 300.
	if u0.ContextTokens != 8800 {
		t.Errorf("turn 0 context = %d, want 8800 (input + cache read + cache write, output excluded)", u0.ContextTokens)
	}
	if u0.CostUSD == nil || *u0.CostUSD < 0.149 || *u0.CostUSD > 0.151 {
		t.Errorf("turn 0 cost = %v, want ~0.15", u0.CostUSD)
	}

	u1 := usage[1]
	if u1.ContextTokens != 100900 { // 900 input + 100000 cache read + 0 cache write
		t.Errorf("turn 1 context = %d, want 100900", u1.ContextTokens)
	}
	// A mixed group returns the priced partial (0.30), not NULL and not a summed-with-zero figure.
	if u1.CostUSD == nil || *u1.CostUSD < 0.299 || *u1.CostUSD > 0.301 {
		t.Errorf("turn 1 cost = %v, want ~0.30 (the priced partial)", u1.CostUSD)
	}
}

// TestSessionTurnUsageUnpricedTurn pins that a turn whose every row is unpriced returns a nil
// cost (unmeasured), never a summed zero that would read as free.
func TestSessionTurnUsageUnpricedTurn(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedForReads(t, st)

	ord := 0
	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Usage: []store.ProjUsage{
			{MessageOrdinal: &ord, Model: "mystery", Input: 100, Output: 50, CostUSD: nil, SourceOffset: 1, SourceIndex: 0},
		},
	}); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	usage, err := st.SessionTurnUsage(ctx, sid)
	if err != nil {
		t.Fatalf("turn usage: %v", err)
	}
	if usage[0].CostUSD != nil {
		t.Errorf("an all-unpriced turn should have nil cost, got %v", *usage[0].CostUSD)
	}
}

// TestMessagesPromptFacts pins that the Messages read surfaces the per-prompt hygiene facts and
// gates them behind the current classifier version: a user prompt classified at
// quality.PromptFactsVersion reads PromptFactsCurrent true with its facts, an assistant message
// carries no facts, and a superseded-version row reads as not current (its facts render as
// nothing).
func TestMessagesPromptFacts(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedForReads(t, st)

	ts := time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC)
	// A terse user prompt (classified live at the current version), then an assistant reply.
	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "fix it", Timestamp: ts},
			{Ordinal: 1, Role: "assistant", Content: "On it.", Model: "gpt-5", Timestamp: ts},
		},
	}); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	// Stamp a third message directly at a superseded facts version to prove the version gate: the
	// row has a digest but an old version, so it must read as not current.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO messages (session_id, ordinal, role, content, prompt_short, prompt_no_code, prompt_digest, prompt_facts_version)
		 VALUES ($1, 2, 'user', 'stale prompt', true, false, 424242, $2)`,
		sid, quality.PromptFactsVersion-1); err != nil {
		t.Fatalf("insert stale-version message: %v", err)
	}

	msgs, err := st.Messages(ctx, sid)
	if err != nil {
		t.Fatalf("read messages: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("read %d messages, want 3", len(msgs))
	}
	// The live-classified terse prompt reads its facts as current.
	if !msgs[0].PromptFactsCurrent {
		t.Error("the live-classified user prompt should read PromptFactsCurrent = true")
	}
	if !msgs[0].PromptShort {
		t.Error("a two-word prompt should classify as short")
	}
	if msgs[0].PromptDigest == 0 {
		t.Error("a classified prompt should carry a non-zero digest")
	}
	// The assistant message carries no facts, so it reads as not current.
	if msgs[1].PromptFactsCurrent {
		t.Error("an assistant message should not read PromptFactsCurrent = true")
	}
	// The superseded-version row reads as not current despite carrying a digest.
	if msgs[2].PromptFactsCurrent {
		t.Error("a message at a superseded facts version should read PromptFactsCurrent = false")
	}
}

// TestToolCallsFileRelPath pins that the ToolCalls read surfaces the worktree-relative path the
// projection derived from the session's cwd, alongside the absolute file_path.
func TestToolCallsFileRelPath(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedForReads(t, st) // cwd is /home/grace/akari

	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "assistant", HasToolUse: true}},
		ToolCalls: []store.ProjToolCall{{
			MessageOrdinal: 0, CallIndex: 0, ToolName: "Edit", Category: "edit",
			FilePath:  "/home/grace/akari/internal/auth.go",
			InputBody: `{"file_path":"internal/auth.go"}`, InputMediaType: "application/json", CallUID: "c1",
		}},
	}); err != nil {
		t.Fatalf("apply delta: %v", err)
	}

	calls, err := st.ToolCalls(ctx, sid)
	if err != nil {
		t.Fatalf("read tool calls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("read %d tool calls, want 1", len(calls))
	}
	if got, want := calls[0].FileRelPath, "internal/auth.go"; got != want {
		t.Errorf("file_rel_path = %q, want %q", got, want)
	}
	if got, want := calls[0].FilePath, "/home/grace/akari/internal/auth.go"; got != want {
		t.Errorf("file_path = %q, want %q (absolute path preserved)", got, want)
	}
}
