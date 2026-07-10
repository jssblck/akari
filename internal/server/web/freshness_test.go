package web

import (
	"testing"
	"time"
)

// snapshotAge bands the snapshot's age into the minute-grain ladder the Insights
// provenance note reads from: generous "just now", then minutes, hours, days.
func TestSnapshotAge(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		age  time.Duration
		want string
	}{
		{"fresh", 5 * time.Second, "updated just now"},
		{"just under the minute band", 89 * time.Second, "updated just now"},
		{"minutes", 12 * time.Minute, "updated 12 min ago"},
		{"just under an hour", 59*time.Minute + 30*time.Second, "updated 59 min ago"},
		{"hours", 3 * time.Hour, "updated 3 hr ago"},
		{"just under the day band", 47 * time.Hour, "updated 47 hr ago"},
		{"first day band", 49 * time.Hour, "updated 2 days ago"},
		{"days", 5 * 24 * time.Hour, "updated 5 days ago"},
	}
	for _, c := range cases {
		if got := snapshotAge(now, now.Add(-c.age)); got != c.want {
			t.Errorf("%s: snapshotAge(%s) = %q, want %q", c.name, c.age, got, c.want)
		}
	}
}
