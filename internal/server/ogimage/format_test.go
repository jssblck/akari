package ogimage

import (
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

func TestLevelFor(t *testing.T) {
	// The sqrt ramp: f = sqrt(v/max), stepped at 0.25/0.5/0.75. max = 100 makes the
	// boundaries land on round values (f>0.75 => v>56.25, etc).
	cases := []struct {
		v, max int64
		want   int
	}{
		{0, 100, 0},   // no usage
		{100, 0, 0},   // no ceiling
		{-5, 100, 0},  // guard against negatives
		{1, 100, 1},   // f=0.1 -> level 1
		{6, 100, 1},   // f=0.245 -> level 1
		{7, 100, 2},   // f=0.264 -> level 2
		{26, 100, 3},  // f=0.51 -> level 3
		{57, 100, 4},  // f=0.755 -> level 4
		{100, 100, 4}, // peak
	}
	for _, c := range cases {
		if got := levelFor(c.v, c.max); got != c.want {
			t.Errorf("levelFor(%d, %d) = %d, want %d", c.v, c.max, got, c.want)
		}
	}
}

func TestGroupThousands(t *testing.T) {
	cases := map[int64]string{
		0:          "0",
		42:         "42",
		999:        "999",
		1000:       "1,000",
		1234567:    "1,234,567",
		1000000000: "1,000,000,000",
	}
	for in, want := range cases {
		if got := groupThousands(in); got != want {
			t.Errorf("groupThousands(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFmtCompact(t *testing.T) {
	cases := map[int64]string{
		0:         "0",
		412:       "412",
		999:       "999",
		1000:      "1.0k",
		63000:     "63.0k",
		1_700_000: "1.7M",
	}
	for in, want := range cases {
		if got := fmtCompact(in); got != want {
			t.Errorf("fmtCompact(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestFmtCost(t *testing.T) {
	cases := []struct {
		usd        float64
		incomplete bool
		want       string
	}{
		{0, false, "$0"},
		{0.0042, false, "$0.0042"},
		{1.5, false, "$1.50"},
		{184.2, false, "$184.20"},
		{184.2, true, "$184.20+"}, // incomplete lower-bound marker
		{0, true, "$0+"},
	}
	for _, c := range cases {
		if got := fmtCost(c.usd, c.incomplete); got != c.want {
			t.Errorf("fmtCost(%v, %v) = %q, want %q", c.usd, c.incomplete, got, c.want)
		}
	}
}

// TestTokenBreakdownCoversAllClasses guards the invariant the card exists to
// satisfy: the caption carries every token class plus the cost, so the standalone
// total is never shown without its breakdown.
func TestTokenBreakdownCoversAllClasses(t *testing.T) {
	a := store.Analytics{
		TotalIn: 284_120_000, TotalOut: 41_900_000,
		TotalCacheRead: 902_000_000, TotalCacheWrite: 8_400_000,
		TotalCost: 184.20,
	}
	got := tokenBreakdown(a)
	for _, want := range []string{"in 284.1M", "out 41.9M", "cache r 902.0M", "cache w 8.4M", "$184.20"} {
		if !contains(got, want) {
			t.Errorf("tokenBreakdown missing %q in %q", want, got)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
