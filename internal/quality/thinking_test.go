package quality

import "testing"

// TestThinkingBucketForTokens pins the absolute token-scale mapping at its edges. Each edge
// is inclusive on the low side (exactly 128 tokens is still low), and a redacted-to-empty
// trace (0 tokens on a turn that did reason) floors at low rather than off, since off is the
// caller's call from the thinking count.
func TestThinkingBucketForTokens(t *testing.T) {
	t.Parallel()
	cases := []struct {
		tokens float64
		want   ThinkingBucket
	}{
		{0, ThinkingLow},
		{1, ThinkingLow},
		{128, ThinkingLow},
		{129, ThinkingMedium},
		{512, ThinkingMedium},
		{513, ThinkingHigh},
		{2048, ThinkingHigh},
		{2049, ThinkingXHigh},
		{50000, ThinkingXHigh},
	}
	for _, c := range cases {
		if got := ThinkingBucketForTokens(c.tokens); got != c.want {
			t.Errorf("ThinkingBucketForTokens(%v) = %s, want %s", c.tokens, got, c.want)
		}
	}
}

// TestThinkingBytesPerToken checks the per-agent calibration divisors and the default. The
// factors differ because each agent's reasoning trace is a different encoding; an unknown
// agent falls back to the Claude factor (the encrypted-signature case).
func TestThinkingBytesPerToken(t *testing.T) {
	t.Parallel()
	if got := ThinkingBytesPerToken("codex"); got != 14.2 {
		t.Errorf("codex factor = %v, want 14.2", got)
	}
	if got := ThinkingBytesPerToken("pi"); got != 4.0 {
		t.Errorf("pi factor = %v, want 4.0", got)
	}
	if got := ThinkingBytesPerToken("claude"); got != 10.7 {
		t.Errorf("claude factor = %v, want 10.7", got)
	}
	if got := ThinkingBytesPerToken("mystery-agent"); got != 10.7 {
		t.Errorf("unknown agent factor = %v, want 10.7 (claude default)", got)
	}
}
