package store_test

import (
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

// TestSignalsContextHealth exercises the whole context-health fact path through the real
// SQL: usage events with a three-bucket prompt size, a subagent (sidechain) turn that must
// be excluded, and a main-thread compaction drop. The peak is the heaviest main-thread turn
// (summing input, cache read, and cache creation), and exactly one reset is counted: the
// main-thread drop, not the dip into the excluded sidechain turn.
func TestSignalsContextHealth(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-context-health")

	// Main-thread context climbs 60k -> 180k -> 200k then compacts to 25k. The 180k turn
	// splits across all three prompt buckets to prove the peak sums them. A sidechain turn
	// (15k) sits between the 180k and 200k main turns: if it were counted, its drop would
	// add a second reset, so excluding it is what keeps the count at one.
	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "do the work"},
			{Ordinal: 1, Role: "assistant", Content: "done"},
		},
		Usage: []store.ProjUsage{
			{Model: "claude-sonnet-4", Input: 60000, SourceOffset: 100, SourceIndex: 0},
			{Model: "claude-sonnet-4", Input: 100000, CacheRead: 60000, CacheWrite: 20000, SourceOffset: 200, SourceIndex: 0},
			{Model: "claude-sonnet-4", Input: 15000, SourceOffset: 250, SourceIndex: 0, IsSidechain: true},
			{Model: "claude-sonnet-4", Input: 200000, SourceOffset: 300, SourceIndex: 0},
			{Model: "claude-sonnet-4", Input: 25000, SourceOffset: 400, SourceIndex: 0},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	setUserMessageCount(t, st, ctx, sid, 1)
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals: %v", err)
	}

	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if !sig.HasContextHealth() {
		t.Fatal("session with main-thread usage should have measured context health")
	}
	if *sig.PeakContextTokens != 200000 {
		t.Errorf("peak context = %d, want 200000", *sig.PeakContextTokens)
	}
	if *sig.ContextResetCount != 1 {
		t.Errorf("context resets = %d, want 1 (the main-thread compaction, not the sidechain dip)", *sig.ContextResetCount)
	}
}

// TestSignalsContextHealthUnmeasured confirms a session with no main-thread usage stores
// NULL for both figures rather than a misleading zero, so the reader can tell "nothing to
// measure" from "measured as zero". Only a sidechain turn is present, so the main-thread
// sequence is empty.
func TestSignalsContextHealthUnmeasured(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "sess-context-unmeasured")

	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "spawn a subagent"},
			{Ordinal: 1, Role: "assistant", Content: "spawning"},
		},
		Usage: []store.ProjUsage{
			{Model: "claude-sonnet-4", Input: 40000, SourceOffset: 100, SourceIndex: 0, IsSidechain: true},
		},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta: %v", err)
	}
	setUserMessageCount(t, st, ctx, sid, 1)
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals: %v", err)
	}

	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if sig.HasContextHealth() {
		t.Errorf("session with only sidechain usage should have no measured context, got peak %v", sig.PeakContextTokens)
	}
	if sig.PeakContextTokens != nil || sig.ContextResetCount != nil {
		t.Errorf("unmeasured context should be NULL, got peak %v resets %v", sig.PeakContextTokens, sig.ContextResetCount)
	}
}
