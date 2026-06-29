package web

import "time"

// DateRange is one option in the overview's trailing-window selector. Days is the
// width of the window; a zero Days means all of history (no lower bound).
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

// rangeSegClass marks the active button in the range selector.
func rangeSegClass(active bool) string {
	if active {
		return "seg active"
	}
	return "seg"
}
