package web_test

import (
	"math"
	"testing"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/web"
)

// TestMessageThinkingBandRoundsLikeSettle pins the boundary case a review flagged: the
// per-message chip must round the token estimate to a whole token before banding, the same rule
// gatherObservedThinking applies to the stored session scalar (math.Round into an int), so a
// one-turn session bands identically in the chip and in the header readout. A Claude turn with
// thinking_bytes=1374 estimates to 1374/10.7 = 128.4 tokens: both paths round to 128 and must
// read low, never the medium the raw float (>128) would give.
func TestMessageThinkingBandRoundsLikeSettle(t *testing.T) {
	t.Parallel()
	const bytes = 1374
	m := store.Message{Role: "assistant", HasThinking: true, ThinkingBytes: bytes}

	got, ok := web.MessageThinkingBand("claude", m)
	if !ok {
		t.Fatal("a turn with a reasoning block should band")
	}
	// The settle pass stores math.Round(bytes/factor) as an int and bands that integer; the chip
	// bands the same rounded value, so the two renderings of one turn agree at every edge.
	want := quality.ThinkingBucketForTokens(math.Round(float64(bytes) / quality.ThinkingBytesPerToken("claude")))
	if got != want {
		t.Errorf("chip band = %s, want %s (the rounded-scalar band the header shows)", got, want)
	}
	if got != quality.ThinkingLow {
		t.Errorf("boundary case bands %s, want low (128.4 rounds to 128)", got)
	}
}

// TestMessageThinkingBandExactAndOff covers the other two paths: Codex's exact per-turn count is
// used directly (preferred over the byte estimate), and a turn with no reasoning block reports
// ok=false so the transcript shows no chip.
func TestMessageThinkingBandExactAndOff(t *testing.T) {
	t.Parallel()

	// Exact 600 reasoning tokens bands high, even though the (ignored) byte weight alone would
	// read xhigh: the reported count wins over the estimate.
	exact := store.Message{Role: "assistant", HasThinking: true, ThinkingBytes: 999999, Usage: &store.TurnUsage{Reasoning: 600}}
	if got, ok := web.MessageThinkingBand("codex", exact); !ok || got != quality.ThinkingHigh {
		t.Errorf("exact 600 tokens -> %s ok=%v, want high", got, ok)
	}

	if _, ok := web.MessageThinkingBand("claude", store.Message{Role: "assistant"}); ok {
		t.Error("a turn with no reasoning block should not band")
	}
}
