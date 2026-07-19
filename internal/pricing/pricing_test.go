package pricing

import (
	"math"
	"testing"
	"time"
)

// anytime is a fixed instant to price single-window models at, where the exact time
// is irrelevant because the model has one rate for all time.
var anytime = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

func TestRateAtDatedSnapshotsAndAliases(t *testing.T) {
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
		// Sonnet at $3/$15 from 3.5 through 4.6. Sonnet 5 is dated and tested apart.
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
		r, ok := RateAt(c.model, anytime)
		if !ok || r.Input != c.input || r.Output != c.output {
			t.Errorf("%s rate = %+v (ok=%v), want input %v / output %v", c.model, r, ok, c.input, c.output)
		}
	}
}

func TestSonnet5DatedWindows(t *testing.T) {
	// Sonnet 5 launched at an introductory $2/$10 through 2026-08-31 and reverts to
	// the $3/$15 sticker on 2026-09-01. The window selects on the event time, and the
	// boundary is inclusive of the sticker window (From is the first sticker instant).
	cases := []struct {
		name                       string
		at                         time.Time
		input, output, write, read float64
	}{
		{"intro launch day", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), 2, 10, 2.50, 0.20},
		{"intro last instant", time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC), 2, 10, 2.50, 0.20},
		{"sticker boundary", sonnet5Sticker, 3, 15, 3.75, 0.30},
		{"sticker later", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 3, 15, 3.75, 0.30},
		// An undated event (zero time) selects the earliest window, the same choice
		// parse-time pricing makes for a row with no OccurredAt.
		{"undated selects intro", time.Time{}, 2, 10, 2.50, 0.20},
	}
	for _, c := range cases {
		r, ok := RateAt("claude-sonnet-5", c.at)
		if !ok {
			t.Errorf("%s: sonnet 5 should be priced", c.name)
			continue
		}
		if r.Input != c.input || r.Output != c.output || r.CacheWrite != c.write || r.CacheRead != c.read {
			t.Errorf("%s: rate = %+v, want %v/%v write %v read %v", c.name, r, c.input, c.output, c.write, c.read)
		}
	}
}

func TestTableWindowsSorted(t *testing.T) {
	// Every model's windows must be From-ascending with a zero-From first window, the
	// shape rateAt relies on: it seeds from the first window and keeps the last whose
	// From is at or before the event time.
	for model, rates := range table {
		if len(rates) == 0 {
			t.Errorf("%s: no rate windows", model)
			continue
		}
		if !rates[0].From.IsZero() {
			t.Errorf("%s: first window From = %v, want the zero value (in effect from the beginning)", model, rates[0].From)
		}
		for i := 1; i < len(rates); i++ {
			if !rates[i].From.After(rates[i-1].From) {
				t.Errorf("%s: window %d From %v is not after window %d From %v", model, i, rates[i].From, i-1, rates[i-1].From)
			}
		}
	}
}

func TestDatedWindowsStartAtUTCMidnight(t *testing.T) {
	// Every non-zero window boundary must fall on UTC midnight. The aggregate cache-savings
	// paths (store/analytics_cache.go) price per UTC day, relying on a whole UTC day sitting
	// inside one rate window so the day-bucketed recompute matches the exact per-row fold it
	// reconciles against. A boundary at any other instant (a midday reprice) would split a day
	// across two windows and make the two disagree. This pins that invariant at the table so a
	// future dated rate cannot silently break it; a genuine midday change would need the
	// aggregate paths reworked to bucket on the exact window first.
	for model, rates := range table {
		for i, dr := range rates {
			if dr.From.IsZero() {
				continue // the open first window is in effect from the beginning, no boundary
			}
			if dr.From.Location() != time.UTC {
				t.Errorf("%s window %d From %v is not in UTC", model, i, dr.From)
			}
			if !dr.From.Equal(dr.From.Truncate(24 * time.Hour)) {
				t.Errorf("%s window %d From %v is not on a UTC-midnight boundary", model, i, dr.From)
			}
		}
	}
}

func TestRateAtFableAndMythos(t *testing.T) {
	// Fable 5, Mythos 5, and the Mythos preview all price at $10/$50.
	for _, model := range []string{
		"claude-fable-5",
		"claude-mythos-5",
		"claude-mythos-preview",
	} {
		r, ok := RateAt(model, anytime)
		if !ok || r.Input != 10 || r.Output != 50 {
			t.Errorf("%s rate = %+v (ok=%v), want input 10 / output 50", model, r, ok)
		}
	}
}

func TestRateAtGPT(t *testing.T) {
	cases := []struct {
		model         string
		input, output float64
	}{
		// Current generation. GPT-5.6 is a three-tier family; the gpt-5.6 alias
		// routes to sol and prices at sol's rate.
		{"gpt-5.6", 5, 30},
		{"gpt-5.6-sol", 5, 30},
		{"gpt-5.6-terra", 2.50, 15},
		{"gpt-5.6-luna", 1, 6},
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
		r, ok := RateAt(c.model, anytime)
		if !ok || r.Input != c.input || r.Output != c.output {
			t.Errorf("%s rate = %+v (ok=%v), want input %v / output %v", c.model, r, ok, c.input, c.output)
		}
	}
}

func TestDateSnapshotNormalization(t *testing.T) {
	// Both date formats strip to the same canonical key; a non-date suffix (a
	// variant) is left intact so it is matched (or not) on its own.
	if r, ok := RateAt("claude-opus-4-8-20260115", anytime); !ok || r.Input != 5 {
		t.Errorf("Anthropic dated form not normalized: %+v (ok=%v)", r, ok)
	}
	if r, ok := RateAt("gpt-5-2025-08-07", anytime); !ok || r.Input != 1.25 {
		t.Errorf("OpenAI dated form not normalized: %+v (ok=%v)", r, ok)
	}
	// A variant suffix is not a date and must not be stripped: gpt-5.4-mini stays
	// gpt-5.4-mini (its own rate), and an unlisted variant stays unknown rather
	// than collapsing onto the base model.
	if r, ok := RateAt("gpt-5.4-mini", anytime); !ok || r.Input != 0.75 {
		t.Errorf("variant suffix wrongly altered: %+v (ok=%v)", r, ok)
	}
}

func TestUnlistedModelsAreUnknown(t *testing.T) {
	// Plausible future or sibling models we have deliberately NOT priced. Each
	// must report unknown rather than inherit a neighbor's rate. Because matching
	// is exact, this now covers same-version
	// variants too (gpt-5.4-turbo no longer collapses onto gpt-5.4), which prefix
	// matching could not guard.
	for _, model := range []string{
		"claude-opus-4-9", "claude-opus-5", "claude-opus-5-0",
		"claude-sonnet-4-7",
		"claude-haiku-4-9", "claude-haiku-5",
		"claude-fable-6", "claude-mythos-6",
		"gpt-5.7", "gpt-6", "gpt-7",
		"gpt-5.4-turbo", "gpt-5.5-ultra", // same-version variants we never priced
		"gpt-5.6-mini", "gpt-5.6-nano", // GPT-5.6's real tiers are sol/terra/luna, not mini/nano
	} {
		if r, ok := RateAt(model, anytime); ok {
			t.Errorf("unlisted model %q priced as %+v; expected unknown", model, r)
		}
	}
}

func TestRateAtUnknown(t *testing.T) {
	if _, ok := RateAt("some-future-model", anytime); ok {
		t.Error("unknown model should not be priced")
	}
	if _, ok := RateAt("", anytime); ok {
		t.Error("empty model should not be priced")
	}
	// A bare date-like string normalizes to empty and must not panic or match.
	if _, ok := RateAt("20250514", anytime); ok {
		t.Error("bare token should not be priced")
	}
}

func TestCost(t *testing.T) {
	// 1M input + 1M output on Sonnet 4.0 (dated ID) at 3 + 15 per million.
	cost := Cost("claude-sonnet-4-20250514", anytime, 1_000_000, 1_000_000, 0, 0)
	if math.Abs(cost-18.0) > 1e-9 {
		t.Errorf("cost = %v, want 18", cost)
	}

	// All four token classes contribute.
	cost = Cost("claude-sonnet-4-5", anytime, 1_000_000, 0, 1_000_000, 1_000_000)
	want := 3.0 + 3.75 + 0.30
	if math.Abs(cost-want) > 1e-9 {
		t.Errorf("cost = %v, want %v", cost, want)
	}

	if got := Cost("mystery-model", anytime, 100, 100, 0, 0); got != 0 {
		t.Errorf("unknown model cost = %v, want 0", got)
	}
}

func TestCostSelectsDatedWindow(t *testing.T) {
	// The same 1M input + 1M output on Sonnet 5 prices at the intro rate inside the
	// promo window and the sticker rate after it.
	intro := Cost("claude-sonnet-5", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), 1_000_000, 1_000_000, 0, 0)
	if math.Abs(intro-12.0) > 1e-9 {
		t.Errorf("intro cost = %v, want 12 (2 + 10)", intro)
	}
	sticker := Cost("claude-sonnet-5", sonnet5Sticker, 1_000_000, 1_000_000, 0, 0)
	if math.Abs(sticker-18.0) > 1e-9 {
		t.Errorf("sticker cost = %v, want 18 (3 + 15)", sticker)
	}
}

func TestCacheSavings(t *testing.T) {
	// Opus 4.8: Input 5, CacheRead 0.50, CacheWrite 6.25 per million.
	// A cache read saves the full input-minus-read gap; a cache write costs the
	// write-minus-input premium (negative saving), the price paid to make reads cheap.
	cases := []struct {
		name        string
		model       string
		read, write int64
		wantSaving  float64
	}{
		// 1M read alone: 1 * (5 - 0.50) = 4.50.
		{"read only", "claude-opus-4-8", 1_000_000, 0, 4.50},
		// 1M write alone: 1 * (5 - 6.25) = -1.25, caching costs more than it saved.
		{"write only is negative", "claude-opus-4-8", 0, 1_000_000, -1.25},
		// Both: 4.50 - 1.25 = 3.25.
		{"read and write net", "claude-opus-4-8", 1_000_000, 1_000_000, 3.25},
		// OpenAI bills no separate cache write (CacheWrite rate is 0), so a read saves
		// the input-minus-read gap and the parser reports no write tokens to charge.
		{"openai read", "gpt-5.5", 1_000_000, 0, 4.50},
		{"unknown model", "secret-model", 1_000_000, 0, 0},
	}
	for _, c := range cases {
		saving := CacheSavings(c.model, anytime, c.read, c.write)
		if math.Abs(saving-c.wantSaving) > 1e-9 {
			t.Errorf("%s: saving = %v, want %v", c.name, saving, c.wantSaving)
		}
	}
}

func TestCacheSavingsSelectsDatedWindow(t *testing.T) {
	// Sonnet 5 intro (Input 2, CacheRead 0.20): 1M read saves 1 * (2 - 0.20) = 1.80.
	// After the sticker (Input 3, CacheRead 0.30): 1M read saves 1 * (3 - 0.30) = 2.70.
	intro := CacheSavings("claude-sonnet-5", time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC), 1_000_000, 0)
	if math.Abs(intro-1.80) > 1e-9 {
		t.Errorf("intro saving = %v, want 1.80", intro)
	}
	sticker := CacheSavings("claude-sonnet-5", sonnet5Sticker, 1_000_000, 0)
	if math.Abs(sticker-2.70) > 1e-9 {
		t.Errorf("sticker saving = %v, want 2.70", sticker)
	}
}
