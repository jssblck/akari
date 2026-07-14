package web

import (
	"testing"
	"time"
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

// TrendBucket aggregates the short windows daily and the long ones weekly, and an
// unknown key falls back to the default range's unit so a stale ?range still
// renders a sane grid.
func TestTrendBucket(t *testing.T) {
	for _, k := range []string{"7d", "30d"} {
		if got := TrendBucket(k); got != "day" {
			t.Errorf("TrendBucket(%q) = %q, want day", k, got)
		}
	}
	for _, k := range []string{"90d", "year", "all"} {
		if got := TrendBucket(k); got != "week" {
			t.Errorf("TrendBucket(%q) = %q, want week", k, got)
		}
	}
	want := TrendBucket(DefaultRange)
	for _, bad := range []string{"", "bogus", "month"} {
		if got := TrendBucket(bad); got != want {
			t.Errorf("TrendBucket(%q) = %q, want default unit %q", bad, got, want)
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
