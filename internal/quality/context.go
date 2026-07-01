package quality

// Context-health thresholds. A "context reset" is inferred, not observed: no agent
// records a compaction marker that akari can read uniformly across Claude, Codex, and
// Pi, so the reset is read from the token stream instead. A turn resets context when its
// prompt size falls to at most resetDropFraction of the prior turn's, and only when the
// prior turn was at least resetKeepFloorTokens large. The floor keeps a drop from a small
// early context (where prompt size still swings as the conversation warms up) from
// reading as a reset; a real compaction or clear sheds a substantial context, so the
// prior turn is always well past it.
//
// The input is main-thread turns only (subagent turns are excluded upstream): on the
// main thread each turn's prompt is the running conversation, which grows turn over turn
// until something sheds it, so a sharp fall is a reliable proxy for a compaction or a
// manual clear. Both constants are absolute token counts, deliberately independent of any
// model's context window, so the signal holds up for a model whose window akari has never
// priced. They are versioned by Version: changing either changes the stored reset counts,
// so a change must bump the signals version.
const (
	resetDropFraction    = 0.5
	resetKeepFloorTokens = 20000
)

// ContextHealth summarizes a session's context load from its ordered per-turn prompt
// sizes (uncached input plus cached read plus cache creation, main-thread turns only, in
// transcript order). Peak is the largest single-turn context the session reached: a
// window-independent "how heavy did it get" score in tokens, where a higher number means
// the session ran closer to whatever its model's limit was. Resets is the count of
// inferred context resets, the sharp drops that read as a compaction or a manual clear.
//
// It is pure so the rule lives in one tested place; the store gathers the ordered sizes
// from usage_events and stores the result on the session's signals row. An empty input
// (a session with no main-thread usage) yields a zero peak and zero resets; the caller
// distinguishes "measured as zero" from "nothing to measure" by whether it had any turns.
func ContextHealth(perTurnTokens []int64) (peak int64, resets int) {
	for i, tokens := range perTurnTokens {
		if tokens > peak {
			peak = tokens
		}
		if i == 0 {
			continue
		}
		prev := perTurnTokens[i-1]
		if prev >= resetKeepFloorTokens && float64(tokens) <= float64(prev)*resetDropFraction {
			resets++
		}
	}
	return peak, resets
}
