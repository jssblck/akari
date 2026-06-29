package pricing

import (
	"math"
	"testing"
)

func TestLookupDatedSnapshotsAndAliases(t *testing.T) {
	// Each model is exercised through its dated release ID and its alias; both
	// must resolve to the same rate via date-snapshot normalization.
	cases := []struct {
		model         string
		input, output float64
	}{
		// Legacy Opus (4.0/4.1) at $15/$75. 4.0's dated ID has no minor number.
		{"claude-opus-4-20250514", 15, 75},
		{"claude-opus-4-0", 15, 75},
		{"claude-opus-4-1-20250805", 15, 75},
		{"claude-opus-4-1", 15, 75},
		// Current Opus (4.5+) at $5/$25.
		{"claude-opus-4-5-20251101", 5, 25},
		{"claude-opus-4-6", 5, 25},
		{"claude-opus-4-7", 5, 25},
		{"claude-opus-4-8", 5, 25},
		// Sonnet at $3/$15 from 3.5 through 4.6.
		{"claude-sonnet-4-20250514", 3, 15},
		{"claude-sonnet-4-0", 3, 15},
		{"claude-sonnet-4-5-20250929", 3, 15},
		{"claude-sonnet-4-6", 3, 15},
		{"claude-3-7-sonnet-20250219", 3, 15},
		{"claude-3-5-sonnet-20241022", 3, 15},
		// Haiku.
		{"claude-haiku-4-5-20251001", 1, 5},
		{"claude-3-5-haiku-20241022", 0.80, 4},
	}
	for _, c := range cases {
		r, ok := Lookup(c.model)
		if !ok || r.Input != c.input || r.Output != c.output {
			t.Errorf("%s rate = %+v (ok=%v), want input %v / output %v", c.model, r, ok, c.input, c.output)
		}
	}
}

func TestLookupFableAndMythos(t *testing.T) {
	// Fable 5, Mythos 5, and the Mythos preview all price at $10/$50.
	for _, model := range []string{
		"claude-fable-5",
		"claude-mythos-5",
		"claude-mythos-preview",
	} {
		r, ok := Lookup(model)
		if !ok || r.Input != 10 || r.Output != 50 {
			t.Errorf("%s rate = %+v (ok=%v), want input 10 / output 50", model, r, ok)
		}
	}
}

func TestLookupGPT(t *testing.T) {
	cases := []struct {
		model         string
		input, output float64
	}{
		// Current generation.
		{"gpt-5.5", 5, 30},
		{"gpt-5.5-pro", 30, 180},
		{"gpt-5.4", 2.50, 15},
		{"gpt-5.4-mini", 0.75, 4.50},
		{"gpt-5.4-nano", 0.20, 1.25},
		{"gpt-5.4-pro", 30, 180},
		{"gpt-5.3-codex", 1.75, 14},
		// Prior generation, including a dated base snapshot that normalizes to gpt-5.
		{"gpt-5", 1.25, 10},
		{"gpt-5-2025-08-07", 1.25, 10},
		{"gpt-5-codex", 1.25, 10},
		{"gpt-5-mini", 0.25, 2},
		{"gpt-5-nano", 0.05, 0.40},
	}
	for _, c := range cases {
		r, ok := Lookup(c.model)
		if !ok || r.Input != c.input || r.Output != c.output {
			t.Errorf("%s rate = %+v (ok=%v), want input %v / output %v", c.model, r, ok, c.input, c.output)
		}
	}
}

func TestDateSnapshotNormalization(t *testing.T) {
	// Both date formats strip to the same canonical key; a non-date suffix (a
	// variant) is left intact so it is matched (or not) on its own.
	if r, ok := Lookup("claude-opus-4-8-20260115"); !ok || r.Input != 5 {
		t.Errorf("Anthropic dated form not normalized: %+v (ok=%v)", r, ok)
	}
	if r, ok := Lookup("gpt-5-2025-08-07"); !ok || r.Input != 1.25 {
		t.Errorf("OpenAI dated form not normalized: %+v (ok=%v)", r, ok)
	}
	// A variant suffix is not a date and must not be stripped: gpt-5.4-mini stays
	// gpt-5.4-mini (its own rate), and an unlisted variant stays unknown rather
	// than collapsing onto the base model.
	if r, ok := Lookup("gpt-5.4-mini"); !ok || r.Input != 0.75 {
		t.Errorf("variant suffix wrongly altered: %+v (ok=%v)", r, ok)
	}
}

func TestUnlistedModelsAreUnknown(t *testing.T) {
	// Plausible future or sibling models we have deliberately NOT priced. Each
	// must report unknown so the cost is flagged incomplete rather than billed at
	// a neighbor's rate. Because matching is exact, this now covers same-version
	// variants too (gpt-5.4-turbo no longer collapses onto gpt-5.4), which prefix
	// matching could not guard.
	for _, model := range []string{
		"claude-opus-4-9", "claude-opus-5", "claude-opus-5-0",
		"claude-sonnet-4-7", "claude-sonnet-5",
		"claude-haiku-4-9", "claude-haiku-5",
		"claude-fable-6", "claude-mythos-6",
		"gpt-5.6", "gpt-6", "gpt-7",
		"gpt-5.4-turbo", "gpt-5.5-ultra", // same-version variants we never priced
	} {
		if r, ok := Lookup(model); ok {
			t.Errorf("unlisted model %q priced as %+v; expected unknown", model, r)
		}
	}
}

func TestLookupUnknown(t *testing.T) {
	if _, ok := Lookup("some-future-model"); ok {
		t.Error("unknown model should not be priced")
	}
	if _, ok := Lookup(""); ok {
		t.Error("empty model should not be priced")
	}
	// A bare date-like string normalizes to empty and must not panic or match.
	if _, ok := Lookup("20250514"); ok {
		t.Error("bare token should not be priced")
	}
}

func TestCost(t *testing.T) {
	// 1M input + 1M output on Sonnet 4.0 (dated ID) at 3 + 15 per million.
	cost, known := Cost("claude-sonnet-4-20250514", 1_000_000, 1_000_000, 0, 0)
	if !known {
		t.Fatal("sonnet should be priced")
	}
	if math.Abs(cost-18.0) > 1e-9 {
		t.Errorf("cost = %v, want 18", cost)
	}

	// All four token classes contribute.
	cost, _ = Cost("claude-sonnet-4-5", 1_000_000, 0, 1_000_000, 1_000_000)
	want := 3.0 + 3.75 + 0.30
	if math.Abs(cost-want) > 1e-9 {
		t.Errorf("cost = %v, want %v", cost, want)
	}

	if _, known := Cost("mystery-model", 100, 100, 0, 0); known {
		t.Error("unknown model cost should report known=false")
	}
}
