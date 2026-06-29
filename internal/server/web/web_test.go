package web

import (
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// RowTokens sums every token class so the Tokens column and its breakdown card
// report the same total the heatmap does.
func TestRowTokens(t *testing.T) {
	s := store.SessionSummary{TotalInput: 100, TotalOutput: 50, TotalCacheRead: 7, TotalCacheWrite: 3}
	if got := RowTokens(s); got != 160 {
		t.Errorf("RowTokens = %d, want 160", got)
	}
}

// relTime buckets the recent past into coarse phrases and falls back to an
// absolute stamp once a relative one stops being useful.
func TestRelTime(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		when time.Time
		want string
	}{
		{"same day earlier", time.Date(2026, 6, 29, 1, 0, 0, 0, time.UTC), "today"},
		{"late last night is a day ago", time.Date(2026, 6, 28, 23, 30, 0, 0, time.UTC), "1 day ago"},
		{"three days", time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC), "3 days ago"},
		{"six days", time.Date(2026, 6, 23, 8, 0, 0, 0, time.UTC), "6 days ago"},
		{"a week falls back to a stamp", time.Date(2026, 6, 22, 8, 0, 0, 0, time.UTC), "2026-06-22 08:00"},
		{"future clock skew reads today", time.Date(2026, 6, 29, 18, 0, 0, 0, time.UTC), "today"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := relTime(now, c.when); got != c.want {
				t.Errorf("relTime(%s) = %q, want %q", c.when.Format(time.RFC3339), got, c.want)
			}
		})
	}
}

// FmtRelTime returns a dash for the absent timestamp rather than panicking on a
// nil pointer or formatting the zero time.
func TestFmtRelTimeAbsent(t *testing.T) {
	if got := FmtRelTime(nil); got != "-" {
		t.Errorf("FmtRelTime(nil) = %q, want %q", got, "-")
	}
	var zero time.Time
	if got := FmtRelTime(&zero); got != "-" {
		t.Errorf("FmtRelTime(zero) = %q, want %q", got, "-")
	}
}
