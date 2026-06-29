package web

import "testing"

// TestReparsePercent covers the progress-bar fill math, including the edges the
// live status can hit: no total yet, a count that briefly exceeds the total, and a
// negative guard.
func TestReparsePercent(t *testing.T) {
	cases := []struct {
		done, total, want int
	}{
		{0, 0, 0},     // total unknown before the scope query returns
		{0, 10, 0},    // nothing done yet
		{5, 10, 50},   // halfway
		{10, 10, 100}, // complete
		{12, 10, 100}, // a late count revision cannot overflow the track
		{-1, 10, 0},   // negative guard
		{5, 0, 0},     // total zero with work reported reads as 0, not a divide
	}
	for _, c := range cases {
		if got := ReparsePercent(c.done, c.total); got != c.want {
			t.Errorf("ReparsePercent(%d, %d) = %d, want %d", c.done, c.total, got, c.want)
		}
	}
}
