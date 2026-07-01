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
// The input is a session's own turns in order: each turn's prompt is the running
// conversation, which grows turn over turn until something sheds it, so a sharp fall is a
// reliable proxy for a compaction or a manual clear. A subagent runs in its own separate
// session, so its turns never mix into another session's sequence. Both constants are
// absolute token counts, deliberately independent of any
// model's context window, so the signal holds up for a model whose window akari has never
// priced. They are versioned by Version: changing either changes the stored reset counts,
// so a change must bump the signals version.
const (
	resetDropFraction    = 0.5
	resetKeepFloorTokens = 20000
)

// ContextHealthFolder computes a session's context-health figures in one streaming pass,
// holding O(1) state (the running peak, the reset count, and the previous turn's size)
// rather than buffering the whole session. The store folds usage rows as they arrive from
// an ordered query, so peak memory does not grow with an arbitrarily long session. Add the
// turns in transcript order; Result reports the peak, the inferred reset count, and whether
// any turn was seen (so the caller can tell "measured as zero" from "nothing to measure").
type ContextHealthFolder struct {
	peak   int64
	resets int
	prev   int64
	seen   bool
}

// Add folds one turn's context size (uncached input plus cached read plus cache creation)
// into the running figures. A reset is inferred when this turn's size falls to at most
// resetDropFraction of the prior turn's and the prior turn was at least resetKeepFloorTokens
// large (see the constants above for why the floor is there).
func (f *ContextHealthFolder) Add(tokens int64) {
	if tokens > f.peak {
		f.peak = tokens
	}
	if f.seen && f.prev >= resetKeepFloorTokens && float64(tokens) <= float64(f.prev)*resetDropFraction {
		f.resets++
	}
	f.prev = tokens
	f.seen = true
}

// Result reports the folded figures. any is false when no turn was added, so the caller
// can store NULL rather than a measured-looking zero for a session with no usage.
func (f *ContextHealthFolder) Result() (peak int64, resets int, any bool) {
	return f.peak, f.resets, f.seen
}

// ContextHealth summarizes a session's context load from its ordered per-turn prompt
// sizes (uncached input plus cached read plus cache creation, in transcript order). Peak is
// the largest single-turn context the session reached: a
// window-independent "how heavy did it get" score in tokens, where a higher number means
// the session ran closer to whatever its model's limit was. Resets is the count of
// inferred context resets, the sharp drops that read as a compaction or a manual clear.
//
// It is the buffered form over ContextHealthFolder, kept as the tested reference and for
// callers that already hold the whole slice; the store folds the streaming form instead so
// its memory stays bounded. An empty input (a session with no usage) yields a zero peak and
// zero resets; the caller distinguishes "measured as zero" from "nothing to measure" by
// whether it had any turns.
func ContextHealth(perTurnTokens []int64) (peak int64, resets int) {
	var f ContextHealthFolder
	for _, tokens := range perTurnTokens {
		f.Add(tokens)
	}
	peak, resets, _ = f.Result()
	return peak, resets
}
