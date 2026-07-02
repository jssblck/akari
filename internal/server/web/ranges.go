package web

import (
	"net/url"
	"time"

	"github.com/jssblck/akari/internal/server/store"
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

// RangeLabel returns a range key's display label ("30 days", "Year"), for the sessions feed's
// active-range chip. An unknown key returns the key itself, so a stale value still reads rather
// than rendering blank.
func RangeLabel(key string) string {
	for _, r := range DateRanges {
		if r.Key == key {
			return r.Label
		}
	}
	return key
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

// rangeSegClass marks the active button in the range selector.
func rangeSegClass(active bool) string {
	if active {
		return "seg active"
	}
	return "seg"
}

// RangeOption is one rendered button in the trailing-window selector: the window
// label, the URL the button refetches the usage panel from, and whether it is the
// active window. The href is built per panel so the selector can sit on any page
// (the global overview or one project) and refetch from that page's own path.
type RangeOption struct {
	Label  string
	Href   string
	Active bool
}

// RangeOptions builds the selector's buttons for a panel rooted at basePath. Any
// params in preserve (except range, which each button sets) ride along on every
// href, so switching the window on a page that also carries other query state
// (the project page's session filters) does not drop that state from the URL.
func RangeOptions(basePath string, preserve url.Values, active string) []RangeOption {
	opts := make([]RangeOption, 0, len(DateRanges))
	for _, dr := range DateRanges {
		q := url.Values{}
		for k, vs := range preserve {
			if k == "range" {
				continue
			}
			for _, v := range vs {
				if v != "" {
					q.Add(k, v)
				}
			}
		}
		q.Set("range", dr.Key)
		opts = append(opts, RangeOption{
			Label:  dr.Label,
			Href:   basePath + "?" + q.Encode(),
			Active: dr.Key == active,
		})
	}
	return opts
}

// ProjectRangeOptions is RangeOptions for a project's usage panel: it roots the
// selector at the project path and preserves the active session filters, so the
// window control and the filter rail share the URL without clobbering each other.
func ProjectRangeOptions(projectID int64, sel store.SessionFilter, active string) []RangeOption {
	preserve := url.Values{}
	if sel.Agent != "" {
		preserve.Set("agent", sel.Agent)
	}
	if sel.Username != "" {
		preserve.Set("user", sel.Username)
	}
	if sel.Machine != "" {
		preserve.Set("machine", sel.Machine)
	}
	return RangeOptions(ProjectPath(projectID), preserve, active)
}
