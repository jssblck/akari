package web

import (
	"fmt"
	"time"
)

// FmtSnapshotAge renders how long ago a precomputed snapshot was taken, minute-grain:
// the Insights page serves from a snapshot recomputed on a cadence rather than live
// data, and this is the provenance note beside its controls. It is deliberately finer
// than FmtRelTime's day-grain ladder: an hour-scale cadence needs minutes to read as
// anything but "today". It reads the wall clock; snapshotAge holds the testable core.
func FmtSnapshotAge(at time.Time) string {
	return snapshotAge(time.Now(), at)
}

// snapshotAge is FmtSnapshotAge's clock-injected core. The first band is generous
// (anything under 90 seconds is "just now") so a page loaded right after a refresh
// does not flash "1 min ago"; past two days the note is evidence the refresher is
// wedged, and it keeps counting rather than capping.
func snapshotAge(now, at time.Time) string {
	d := now.Sub(at)
	switch {
	case d < 90*time.Second:
		return "updated just now"
	case d < time.Hour:
		return fmt.Sprintf("updated %d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		// Hours run to 47 so the days band below always pluralizes.
		return fmt.Sprintf("updated %d hr ago", int(d.Hours()))
	}
	return fmt.Sprintf("updated %d days ago", int(d.Hours()/24))
}
