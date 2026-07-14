package web

import (
	"time"
)

// DateRange is one option in the trailing-window selector. Days is the width of
// the window; a zero Days means all of history (no lower bound).
type DateRange struct {
	Key   string
	Label string
	Days  int
}

// DateRanges are the windows the overview offers, narrowest first. The selector
// renders them in this order, and ParseRange validates against their keys.
var DateRanges = []DateRange{
	{Key: "7d", Label: "7 days", Days: 7},
	{Key: "30d", Label: "30 days", Days: 30},
	{Key: "90d", Label: "90 days", Days: 90},
	{Key: "year", Label: "Year", Days: 365},
	{Key: "all", Label: "All", Days: 0},
}

// DefaultRange is the window the overview opens on: a trailing year, wide enough
// to read seasonality and longer trends on the activity grid without jumping to
// all of history.
const DefaultRange = "year"

// ParseRange normalizes a range query value to a known key, falling back to the
// default for anything empty or unrecognized.
func ParseRange(key string) string {
	for _, r := range DateRanges {
		if r.Key == key {
			return key
		}
	}
	return DefaultRange
}

// RangeBounds reports whether a range key names a bounded trailing window (a known key with a
// positive day span). It is false for "all", the empty string, and any unknown value, which the
// sessions feed treats as all-history: those add no ?range param, keeping the bare feed unbounded.
// It is the whitelist the feed's range param passes through (like IsOutcome / IsGrade for
// their params), so a stale or hand-edited ?range never bounds the list to a made-up window.
func RangeBounds(key string) bool {
	for _, r := range DateRanges {
		if r.Key == key {
			return r.Days > 0
		}
	}
	return false
}

// RangeSince converts a range key to the lower time bound to pass to the store,
// measured back from now. The "all" window (zero Days) returns the zero time,
// which the store reads as "no bound".
func RangeSince(key string, now time.Time) time.Time {
	for _, r := range DateRanges {
		if r.Key == key && r.Days > 0 {
			return now.AddDate(0, 0, -r.Days)
		}
	}
	return time.Time{}
}

// TrendBucket picks the time-bucket unit the Insights trend charts aggregate a range
// into: daily for the short windows (7d/30d) where a day still carries enough sessions
// to read, weekly for the long windows (90d/year/all) where daily points would be noise.
// The choice is the same for every chart in a view, so all the trend series share one
// bucket grid and the range selector windows them together. An unknown key falls back to
// the default range's unit, so a stale ?range still renders a sane grid.
func TrendBucket(key string) string {
	switch key {
	case "7d", "30d":
		return "day"
	case "90d", "year", "all":
		return "week"
	}
	return TrendBucket(DefaultRange)
}
