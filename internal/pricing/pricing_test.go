package pricing

import (
	"math"
	"testing"
)

func TestLookupLongestPrefix(t *testing.T) {
	// gpt-5-codex is more specific than gpt-5 and must win.
	r, ok := Lookup("gpt-5-codex-2025")
	if !ok {
		t.Fatal("expected gpt-5-codex to be priced")
	}
	if r.Input != 1.25 || r.Output != 10 {
		t.Errorf("gpt-5-codex rate = %+v", r)
	}

	r, ok = Lookup("claude-opus-4-20250514")
	if !ok {
		t.Fatal("expected claude-opus-4 to be priced")
	}
	if r.Output != 75 {
		t.Errorf("opus output rate = %v, want 75", r.Output)
	}
}

func TestLookupUnknown(t *testing.T) {
	if _, ok := Lookup("some-future-model"); ok {
		t.Error("unknown model should not be priced")
	}
	if _, ok := Lookup(""); ok {
		t.Error("empty model should not be priced")
	}
}

func TestCost(t *testing.T) {
	// 1M input + 1M output on sonnet-4 at 3 + 15 per million.
	cost, known := Cost("claude-sonnet-4-20250514", 1_000_000, 1_000_000, 0, 0)
	if !known {
		t.Fatal("sonnet should be priced")
	}
	if math.Abs(cost-18.0) > 1e-9 {
		t.Errorf("cost = %v, want 18", cost)
	}

	// All four token classes contribute.
	cost, _ = Cost("claude-sonnet-4", 1_000_000, 0, 1_000_000, 1_000_000)
	want := 3.0 + 3.75 + 0.30
	if math.Abs(cost-want) > 1e-9 {
		t.Errorf("cost = %v, want %v", cost, want)
	}

	if _, known := Cost("mystery-model", 100, 100, 0, 0); known {
		t.Error("unknown model cost should report known=false")
	}
}
