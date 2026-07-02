package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// ParseRange passes through every known key and falls back to the default for
// anything empty or unrecognized, so a hand-typed ?range= never 500s the page.
func TestParseRange(t *testing.T) {
	for _, dr := range DateRanges {
		if got := ParseRange(dr.Key); got != dr.Key {
			t.Errorf("ParseRange(%q) = %q, want passthrough", dr.Key, got)
		}
	}
	for _, bad := range []string{"", "bogus", "7", "month", "30D"} {
		if got := ParseRange(bad); got != DefaultRange {
			t.Errorf("ParseRange(%q) = %q, want default %q", bad, got, DefaultRange)
		}
	}
}

// RangeSince turns a bounded key into now minus its day span, and leaves the all
// window (and any unknown key) unbounded as the zero time the store reads as "no
// lower bound".
func TestRangeSince(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	bounded := map[string]int{"7d": 7, "30d": 30, "90d": 90, "year": 365}
	for key, days := range bounded {
		want := now.AddDate(0, 0, -days)
		if got := RangeSince(key, now); !got.Equal(want) {
			t.Errorf("RangeSince(%q) = %v, want %v", key, got, want)
		}
	}
	for _, unbounded := range []string{"all", "bogus", ""} {
		if got := RangeSince(unbounded, now); !got.IsZero() {
			t.Errorf("RangeSince(%q) = %v, want zero", unbounded, got)
		}
	}
}

// RangeBounds is the sessions feed's whitelist: only a known trailing window bounds the
// list, so an "all", empty, or hand-typed junk key leaves the feed unbounded rather than
// falling to ParseRange's trailing-year default.
func TestRangeBounds(t *testing.T) {
	for _, k := range []string{"7d", "30d", "90d", "year"} {
		if !RangeBounds(k) {
			t.Errorf("RangeBounds(%q) = false, want true (bounded window)", k)
		}
	}
	for _, k := range []string{"all", "", "bogus", "month"} {
		if RangeBounds(k) {
			t.Errorf("RangeBounds(%q) = true, want false (unbounded)", k)
		}
	}
}

// RangeOptions builds one button per window, each refetching basePath at its own
// range. Anything in preserve rides along except an incoming range (each button
// sets its own) and empty values (dropped), so a stray ?range= or a blank filter
// never doubles or litters the href.
func TestRangeOptions(t *testing.T) {
	preserve := url.Values{
		"range": {"7d"},     // dropped: every button sets the window itself
		"agent": {"claude"}, // preserved
		"user":  {""},       // dropped: empty value
	}
	opts := RangeOptions("/projects/7", preserve, "30d")
	if len(opts) != len(DateRanges) {
		t.Fatalf("want %d options, got %d", len(DateRanges), len(opts))
	}
	var active []RangeOption
	for _, o := range opts {
		if strings.Contains(o.Href, "user=") {
			t.Errorf("empty filter value should be dropped: %s", o.Href)
		}
		if n := strings.Count(o.Href, "range="); n != 1 {
			t.Errorf("href should carry exactly one range, got %d: %s", n, o.Href)
		}
		if o.Active {
			active = append(active, o)
		}
	}
	if len(active) != 1 {
		t.Fatalf("exactly one window is active, got %d", len(active))
	}
	// The active (30d) button preserves the real filter and sets its own window.
	if active[0].Href != "/projects/7?agent=claude&range=30d" {
		t.Errorf("active href = %q", active[0].Href)
	}
}

// ProjectRangeOptions preserves all three session filters (agent, user, machine),
// not just agent, so switching the window on a filtered project list holds the
// whole selection. url.Values.Encode sorts the keys, so the order is stable.
func TestProjectRangeOptionsPreservesAllFilters(t *testing.T) {
	sel := store.SessionFilter{ProjectID: 7, Agent: "claude", Username: "grace", Machine: "rig"}
	opts := ProjectRangeOptions(7, sel, "90d")
	var active *RangeOption
	for i := range opts {
		if opts[i].Active {
			active = &opts[i]
		}
	}
	if active == nil {
		t.Fatal("no active option for 90d")
	}
	if active.Href != "/projects/7?agent=claude&machine=rig&range=90d&user=grace" {
		t.Errorf("active href = %q", active.Href)
	}
}
