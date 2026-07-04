package store_test

import (
	"testing"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
)

// thinkingSession seeds one settled, refreshed claude session whose assistant turns carry the
// given reasoning-trace weights in bytes (zero means a turn with no thinking), all attributed
// to model. A nonzero weight with no thinking text is exactly the redacted shape current
// agents emit, which is what the signal must measure. It returns the session id ready for
// reads: signals refreshed and the session out of the stale set.
//
// The session's agent is claude (seedSession's default), so a byte weight maps to tokens
// through the claude divisor (quality.ThinkingBytesPerToken("claude") == 10.7). Tests choose
// byte weights that land on round token counts: 10.7 * N bytes estimates to N tokens.
func thinkingSession(t *testing.T, st *store.Store, uid, pid int64, source, model string, thinkingSizes []int) int64 {
	t.Helper()
	ctx := t.Context()
	sid := seedSession(t, st, uid, pid, source)
	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "user", Content: "do the work"}},
	}
	for i, size := range thinkingSizes {
		m := store.MessageDelta{Ordinal: i + 1, Role: "assistant", Content: "done", Model: model}
		if size > 0 {
			m.HasThinking = true
			m.ThinkingBytes = size
		}
		delta.Messages = append(delta.Messages, m)
	}
	rebuildWith(t, st, sid, delta)
	settleSession(t, st, ctx, sid)
	if err := st.RefreshSessionSignals(ctx, sid); err != nil {
		t.Fatalf("refresh signals: %v", err)
	}
	return sid
}

// bytesForTokens is the claude-agent byte weight that estimates to exactly tok tokens, so a
// test can seed a turn at a chosen point on the absolute token scale. It is the inverse of the
// perTurnTokensExpr estimate for a claude turn (bytes / 10.7), and 10.7 * tok is an integer for
// the round token counts the tests use.
func bytesForTokens(tok int) int {
	return int(quality.ThinkingBytesPerToken("claude") * float64(tok))
}

// TestSignalsObservedThinking exercises the observed-thinking fact path through the real SQL:
// the assistant and thinking turn counts, and the per-turn tail and peak token scalars derived
// from the messages.thinking_bytes column. The band reads off the tail on the absolute scale.
func TestSignalsObservedThinking(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	// Three assistant turns: 100 tokens of thinking, then an off turn, then 300 tokens. The two
	// thinking turns' top decile is a single turn (the 300), so both tail and peak read 300.
	sid := thinkingSession(t, st, uid, pid, "sess-thinking", "claude-fable-5",
		[]int{bytesForTokens(100), 0, bytesForTokens(300)})

	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if !sig.HasThinkingMeasure() {
		t.Fatal("session with assistant turns should have a thinking measurement")
	}
	if *sig.AssistantTurns != 3 {
		t.Errorf("assistant turns = %d, want 3", *sig.AssistantTurns)
	}
	if *sig.ThinkingTurns != 2 {
		t.Errorf("thinking turns = %d, want 2", *sig.ThinkingTurns)
	}
	if *sig.ThinkingTailTokens != 300 {
		t.Errorf("tail tokens = %d, want 300", *sig.ThinkingTailTokens)
	}
	if *sig.ThinkingPeakTokens != 300 {
		t.Errorf("peak tokens = %d, want 300", *sig.ThinkingPeakTokens)
	}
	if got := sig.ThinkingBucket(); got != quality.ThinkingMedium {
		t.Errorf("band = %s, want medium (300 tokens)", got)
	}
	if got, want := sig.ThinkingCoverage(), 2.0/3.0; got != want {
		t.Errorf("coverage = %v, want %v", got, want)
	}
}

// TestSignalsObservedThinkingTailIsDecileMean pins the tail as a decile mean distinct from the
// peak, the reason the session scalar is a tail statistic rather than a bare max: one heavy turn
// among many light ones lifts the peak into a high band while the hardest-decile mean stays
// moderate, so the band reads "how hard it thought when it thought hard" without a single outlier
// defining the session.
func TestSignalsObservedThinkingTailIsDecileMean(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	// Twelve thinking turns: eleven at 100 tokens and one at 3000. The top decile is
	// ceil(12 * 0.1) = 2 turns, so the tail is the mean of {3000, 100} = 1550 (a high band),
	// while the peak is the lone 3000 (very high).
	sizes := make([]int, 0, 12)
	for i := 0; i < 11; i++ {
		sizes = append(sizes, bytesForTokens(100))
	}
	sizes = append(sizes, bytesForTokens(3000))
	sid := thinkingSession(t, st, uid, pid, "sess-thinking-tail", "claude-fable-5", sizes)

	sig, err := st.SessionSignalsByID(ctx, sid)
	if err != nil {
		t.Fatalf("read signals: %v", err)
	}
	if *sig.ThinkingTurns != 12 {
		t.Errorf("thinking turns = %d, want 12", *sig.ThinkingTurns)
	}
	if *sig.ThinkingTailTokens != 1550 {
		t.Errorf("tail tokens = %d, want 1550 (mean of top decile {3000,100})", *sig.ThinkingTailTokens)
	}
	if *sig.ThinkingPeakTokens != 3000 {
		t.Errorf("peak tokens = %d, want 3000", *sig.ThinkingPeakTokens)
	}
	if got := sig.ThinkingBucket(); got != quality.ThinkingHigh {
		t.Errorf("band = %s, want high (tail 1550 tokens)", got)
	}
}

// TestSignalsObservedThinkingOffAndUnmeasured pins the two zero-adjacent states apart: a session
// whose assistant turns never thought stores a measured zero (off), while a session with no
// assistant turns at all stores NULL (nothing to measure), so the UI can tell "thinking off" from
// "no read".
func TestSignalsObservedThinkingOffAndUnmeasured(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)

	off := thinkingSession(t, st, uid, pid, "sess-thinking-off", "claude-fable-5", []int{0, 0})
	sig, err := st.SessionSignalsByID(ctx, off)
	if err != nil {
		t.Fatalf("read off signals: %v", err)
	}
	if !sig.HasThinkingMeasure() {
		t.Fatal("off session should still be measured")
	}
	if *sig.ThinkingTurns != 0 || *sig.ThinkingTailTokens != 0 || *sig.ThinkingPeakTokens != 0 {
		t.Errorf("off session should store zero turns and volume, got turns %d tail %d peak %d",
			*sig.ThinkingTurns, *sig.ThinkingTailTokens, *sig.ThinkingPeakTokens)
	}
	if got := sig.ThinkingBucket(); got != quality.ThinkingOff {
		t.Errorf("off session band = %s, want off", got)
	}

	bare := thinkingSession(t, st, uid, pid, "sess-thinking-unmeasured", "", nil)
	sig, err = st.SessionSignalsByID(ctx, bare)
	if err != nil {
		t.Fatalf("read unmeasured signals: %v", err)
	}
	if sig.HasThinkingMeasure() {
		t.Errorf("session with no assistant turns should be unmeasured, got %v turns", sig.AssistantTurns)
	}
}
