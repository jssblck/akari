package store_test

import (
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

// TestSignalsContextHealth exercises the whole context-health fact path through the real
// SQL: usage events with a three-bucket prompt size and a compaction drop. The peak is the
// heaviest turn (summing input, cache read, and cache creation), and exactly one reset is
// counted at the sharp fall. A session's usage is one coherent context (a subagent is a
// separate session), so every usage turn is read with no carve-out.
func TestSignalsContextHealth(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-context-health")

	// Context climbs 60k -> 180k -> 200k then compacts to 25k. The 180k turn splits across
	// all three prompt buckets to prove the peak sums them; the 200k -> 25k fall is the one
	// reset.
	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "do the work"},
			{Ordinal: 1, Role: "assistant", Content: "done"},
		},
		Usage: []store.ProjUsage{
			{Model: "claude-sonnet-4", Input: 60000, SourceOffset: 100, SourceIndex: 0},
			{Model: "claude-sonnet-4", Input: 100000, CacheRead: 60000, CacheWrite: 20000, SourceOffset: 200, SourceIndex: 0},
			{Model: "claude-sonnet-4", Input: 200000, SourceOffset: 300, SourceIndex: 0},
			{Model: "claude-sonnet-4", Input: 25000, SourceOffset: 400, SourceIndex: 0},
		},
	}
	rebuildWith(t, st, sid, delta)
	settleSession(t, st, ctx, sid)
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals: %v", err)
	}

	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if !sig.HasContextHealth() {
		t.Fatal("session with usage should have measured context health")
	}
	if *sig.PeakContextTokens != 200000 {
		t.Errorf("peak context = %d, want 200000", *sig.PeakContextTokens)
	}
	if *sig.ContextResetCount != 1 {
		t.Errorf("context resets = %d, want 1 (the compaction drop)", *sig.ContextResetCount)
	}
}

// TestSignalsContextHealthUnmeasured confirms a session with no usage stores NULL for both
// figures rather than a misleading zero, so the reader can tell "nothing to measure" from
// "measured as zero". A pure-conversation session carries messages but no usage events, so
// the turn sequence is empty.
func TestSignalsContextHealthUnmeasured(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-context-unmeasured")

	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "just chatting"},
			{Ordinal: 1, Role: "assistant", Content: "hello"},
		},
	}
	rebuildWith(t, st, sid, delta)
	settleSession(t, st, ctx, sid)
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals: %v", err)
	}

	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if sig.HasContextHealth() {
		t.Errorf("session with no usage should have no measured context, got peak %v", sig.PeakContextTokens)
	}
	if sig.PeakContextTokens != nil || sig.ContextResetCount != nil {
		t.Errorf("unmeasured context should be NULL, got peak %v resets %v", sig.PeakContextTokens, sig.ContextResetCount)
	}
}
