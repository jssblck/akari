// Package web holds akari's server-rendered UI: templ templates and the small
// view-model helpers they use. Handlers in the httpapi package resolve auth,
// query the store, and render these templates, so all rendering lives here.
package web

import (
	"math"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
)

// ThinkingReadout is what the Quality tooltip's Thinking group renders: whether the session
// was measured at all (it had assistant turns), the absolute band its observed thinking
// landed in (quality.ThinkingBucketForTokens over the hardest-decile-mean turn; ThinkingOff
// when no turn carried a reasoning block), and the estimated per-turn token volumes behind
// it. Tail is the band's basis (the mean of the hardest tenth of the thinking turns), Peak
// the single hardest turn, and Coverage the share of assistant turns that reasoned.
type ThinkingReadout struct {
	Measured   bool
	Bucket     quality.ThinkingBucket
	Turns      int     // assistant turns that carried a reasoning block
	TailTokens int     // hardest-decile-mean per-turn reasoning tokens (the band's basis)
	PeakTokens int     // the single hardest turn's reasoning tokens
	Coverage   float64 // share of assistant turns that reasoned, in [0, 1]
}

// ThinkingBucketLabel renders a band for the session tooltip and the per-message transcript
// chip. The xhigh constant reads better spelled out.
func ThinkingBucketLabel(b quality.ThinkingBucket) string {
	if b == quality.ThinkingXHigh {
		return "very high"
	}
	return string(b)
}

// ThinkingTokensLabel renders an estimated per-turn reasoning-token volume ("~300 tok"). The
// tilde marks it as an estimate: Codex reasoning tokens are exact, but the Claude and pi
// figures are inferred from the reasoning-trace bytes, so the readout never implies a
// precision it does not have. The scale is absolute and agent-independent, so the figure is
// comparable across models (unlike the first cut's within-model byte proxy). The per-turn
// framing comes from the surrounding label, so it is left off here.
func ThinkingTokensLabel(tokens int) string {
	return "~" + FmtTokensCompact(int64(tokens)) + " tok"
}

// MessageThinkingBand is the in-transcript per-message band: the absolute thinking band for
// one assistant turn, rendered as a chip before the message so the reader sees how hard the
// model deliberated on that turn without opening the (often redacted) trace. ok is false for
// a turn that carried no reasoning block, so the caller shows no chip. The turn's tokens are
// its exact reasoning count where the agent reports one (Codex, in the turn usage) else its
// reasoning-trace bytes over the agent's calibrated factor.
//
// The estimate is rounded to whole tokens BEFORE banding, the same rule the settle pass
// applies in gatherObservedThinking (math.Round into the stored int scalar) so a one-turn
// session bands identically in the chip and in the header readout. Banding the raw float here
// would let a turn whose estimate lands just over an edge (thinking_bytes=1374 for Claude is
// 128.4 tokens) read one band in the chip and another off the rounded scalar (128 -> low).
func MessageThinkingBand(agent string, m store.Message) (quality.ThinkingBucket, bool) {
	if !m.HasThinking {
		return quality.ThinkingOff, false
	}
	var tokens float64
	if m.Usage != nil && m.Usage.Reasoning > 0 {
		tokens = float64(m.Usage.Reasoning)
	} else {
		tokens = float64(m.ThinkingBytes) / quality.ThinkingBytesPerToken(agent)
	}
	return quality.ThinkingBucketForTokens(math.Round(tokens)), true
}
