package web

import (
	"testing"
	"time"
)

// relTimeFrom buckets activity into coarse relative phrases for the first week
// and falls back to an absolute stamp beyond it. The reference "now" is fixed so
// the boundaries (today, one day, the week edge, and just past it) are exact.
func TestRelTimeFrom(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		t    time.Time
		want string
	}{
		{"earlier today", now.Add(-3 * time.Hour), "today"},
		{"start of today", time.Date(2026, 6, 29, 0, 5, 0, 0, time.UTC), "today"},
		{"future skew reads as today", now.Add(2 * time.Hour), "today"},
		{"yesterday", time.Date(2026, 6, 28, 23, 0, 0, 0, time.UTC), "1 day ago"},
		{"five days", time.Date(2026, 6, 24, 9, 0, 0, 0, time.UTC), "5 days ago"},
		{"week edge stays relative", time.Date(2026, 6, 22, 9, 0, 0, 0, time.UTC), "7 days ago"},
		{"past a week shows stamp", time.Date(2026, 6, 21, 9, 30, 0, 0, time.UTC), "2026-06-21 09:30"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := relTimeFrom(c.t, now); got != c.want {
				t.Errorf("relTimeFrom(%s) = %q, want %q", c.t.Format(time.RFC3339), got, c.want)
			}
		})
	}
}

// FmtRelTime guards the nil/zero case the projects table can hand it (a project
// with no recorded activity), where a dash reads cleaner than a bogus "today".
func TestFmtRelTimeEmpty(t *testing.T) {
	if got := FmtRelTime(nil); got != "-" {
		t.Errorf("FmtRelTime(nil) = %q, want %q", got, "-")
	}
	var zero time.Time
	if got := FmtRelTime(&zero); got != "-" {
		t.Errorf("FmtRelTime(zero) = %q, want %q", got, "-")
	}
}
