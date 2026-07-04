package quality

// Observed thinking is how much a session's model actually deliberated, read from the
// reasoning trace the agents log. It is deliberately NOT the configured thinking level:
// no agent's transcript records the setting uniformly, and the observed volume is the
// more useful figure anyway. It answers "how hard did the model have to think here",
// which is the setting times the task's real difficulty: a high-effort session of
// trivial turns reads low.
//
// The trace is often redacted. Current Claude Code and Codex ship the reasoning
// encrypted (Claude leaves a "signature", Codex an "encrypted_content" blob) and drop
// the plaintext, so there is no thinking text to measure; pi keeps its thinking in the
// clear. The parser records each turn's reasoning-trace weight in bytes (plaintext where
// present, else the encrypted payload length; see parser.Message.ThinkingBytes), and
// this package turns that into an estimated reasoning-token count.
//
// The unit is per-turn estimated reasoning tokens. Codex reports its exact
// reasoning-token count per turn (message_turn_usage.reasoning_tokens), which is used
// directly; for Claude and pi the token count is estimated from the trace bytes with an
// agent-specific bytes-per-token factor. The estimate is trustworthy: measured against
// Codex's exact counts and against the rare Claude blocks that kept their plaintext, the
// per-turn medians agree within ~2%, so a token figure is comparable across models
// without the per-model ranking the first cut used.
//
// The band is an absolute cut on that token scale, not a per-model quartile. Quartiles
// are 25%-each by construction, so a fleet distribution over them is tautological; an
// absolute scale tracks the real spread and shifts when behavior shifts.
// The edges are baked constants rather than recomputed at read time, so a stored or
// displayed band is stable until an intentional rescale. A change to a bytes-per-token
// factor moves stored scalars and so rides a parse.Epoch bump; an edge change is applied
// at read time and needs no bump.

// ThinkingBucket is the banded read of an observed-thinking volume.
type ThinkingBucket string

const (
	// ThinkingOff: no thinking at all. Decided by the caller from the turn or session's
	// thinking count, never from a token figure, so a turn that thought only a little
	// reads low rather than off.
	ThinkingOff    ThinkingBucket = "off"
	ThinkingLow    ThinkingBucket = "low"
	ThinkingMedium ThinkingBucket = "medium"
	ThinkingHigh   ThinkingBucket = "high"
	ThinkingXHigh  ThinkingBucket = "xhigh"
)

// The band edges over per-turn estimated reasoning tokens. A turn (or a session's
// hardest-decile-mean turn) sits in the band its token count reaches. The edges are a
// rough log2 ladder (128, 512, 2048) chosen from the observed distribution so the per-turn
// population spreads across the bands rather than piling into one, the failure mode of the
// budget-anchored tiers (models almost never spend their configured budget, so tiers keyed
// to it put ~everything in the bottom band).
const (
	ThinkingLowMaxTokens    = 128  // low:    (0, 128]
	ThinkingMediumMaxTokens = 512  // medium: (128, 512]
	ThinkingHighMaxTokens   = 2048 // high:   (512, 2048], xhigh above
)

// thinkingBytesPerToken is the agent-specific divisor turning reasoning-trace bytes into an
// estimated token count. The factors differ because the trace is a different encoding per
// agent: Claude ships an encrypted "signature" (~10.7 bytes per plaintext token, measured
// against the blocks that kept their plaintext), Codex an "encrypted_content" blob (~14.2
// bytes per reasoning token, measured against the exact counts it also reports), and pi
// keeps plaintext (~4 bytes per token, the usual English ratio). Codex's exact per-turn
// count is used directly where present, so its factor only covers a Codex turn that
// somehow logged a trace but no token count.
var thinkingBytesPerToken = map[string]float64{
	"claude": 10.7,
	"codex":  14.2,
	"pi":     4.0,
}

// ThinkingBytesPerToken returns the bytes-per-token divisor for an agent, defaulting to the
// Claude factor for an unknown agent (the encrypted-signature case is the common one). The
// store uses it to build the per-turn token expression shared by the settle derivation and
// the fleet aggregate.
func ThinkingBytesPerToken(agent string) float64 {
	if f, ok := thinkingBytesPerToken[agent]; ok {
		return f
	}
	return thinkingBytesPerToken["claude"]
}

// ThinkingBucketForTokens maps an estimated per-turn reasoning-token count to its band. It
// maps a thinking turn only (tokens from a turn that reasoned): the off case is the
// caller's, decided from the thinking count, so a turn that thought only a few tokens reads
// low rather than off. A non-positive count with a reasoning block present (a redacted-to-
// empty trace) still reads low, the floor of the thinking bands.
func ThinkingBucketForTokens(tokens float64) ThinkingBucket {
	switch {
	case tokens <= ThinkingLowMaxTokens:
		return ThinkingLow
	case tokens <= ThinkingMediumMaxTokens:
		return ThinkingMedium
	case tokens <= ThinkingHighMaxTokens:
		return ThinkingHigh
	default:
		return ThinkingXHigh
	}
}
