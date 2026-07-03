package store

import (
	"math"
	"testing"
	"time"
)

// The bucket grid must agree with Postgres date_trunc: days truncate to midnight UTC, weeks
// anchor on Monday. The Go truncation and the SQL date_trunc have to land a scanned bucket
// start on the same grid position, so this pins both cases.
func TestTruncBucket(t *testing.T) {
	// A day truncates to midnight UTC regardless of the time of day.
	got := truncBucket("day", time.Date(2026, 6, 3, 15, 4, 5, 0, time.UTC))
	want := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("day trunc = %v, want %v", got, want)
	}

	// A week anchors on the Monday at or before the instant, for every day of a known week
	// (Mon 2026-06-01 through Sun 2026-06-07).
	monday := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if monday.Weekday() != time.Monday {
		t.Fatalf("test fixture wrong: 2026-06-01 is %v, not Monday", monday.Weekday())
	}
	for d := 0; d < 7; d++ {
		instant := time.Date(2026, 6, 1+d, 9, 0, 0, 0, time.UTC)
		if got := truncBucket("week", instant); !got.Equal(monday) {
			t.Errorf("week trunc of %v = %v, want %v", instant, got, monday)
		}
	}
}

// newTrendGrid spans [since, until] at the unit, labels each bucket, and indexes a scanned
// start back to its position (or -1 outside the grid).
func TestNewTrendGrid(t *testing.T) {
	since := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	until := time.Date(2026, 6, 4, 20, 0, 0, 0, time.UTC)
	g := newTrendGrid("day", since, until)

	if g.n() != 4 {
		t.Fatalf("grid has %d buckets, want 4 (Jun 1..4)", g.n())
	}
	if got := g.index(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)); got != 0 {
		t.Errorf("index(Jun 1) = %d, want 0", got)
	}
	if got := g.index(time.Date(2026, 6, 4, 23, 59, 0, 0, time.UTC)); got != 3 {
		t.Errorf("index(Jun 4) = %d, want 3", got)
	}
	if got := g.index(time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)); got != -1 {
		t.Errorf("index(before grid) = %d, want -1", got)
	}
	if labels := g.labels(); len(labels) != 4 || labels[0] != "Jun 1" || labels[3] != "Jun 4" {
		t.Errorf("labels = %v, want [Jun 1 .. Jun 4]", labels)
	}
}

// An unbounded window is capped to the most recent buckets so the payload stays bounded, and
// the last bucket still lands on the truncated upper bound. The first bucket lands exactly
// maxTrendBuckets-1 buckets before it: the grid is built from that capped start rather than
// walked from the years-back earliest session and trimmed, so its size is bounded by the cap,
// not by the corpus age.
func TestNewTrendGridCaps(t *testing.T) {
	until := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	since := until.AddDate(-3, 0, 0) // three years of days, well over the cap
	g := newTrendGrid("day", since, until)

	if g.n() != maxTrendBuckets {
		t.Errorf("grid has %d buckets, want the cap %d", g.n(), maxTrendBuckets)
	}
	if last := g.Starts[g.n()-1]; !last.Equal(truncBucket("day", until)) {
		t.Errorf("last bucket = %v, want the truncated upper bound %v", last, truncBucket("day", until))
	}
	wantFirst := truncBucket("day", until).AddDate(0, 0, -(maxTrendBuckets - 1))
	if first := g.Starts[0]; !first.Equal(wantFirst) {
		t.Errorf("first bucket = %v, want %v (maxTrendBuckets-1 days before the last)", first, wantFirst)
	}
}

// foldCategoryMix keeps the busiest categories as their own bands, folds the tail into
// "other" (always last), and normalizes each bucket to percent.
func TestFoldCategoryMix(t *testing.T) {
	// Eight categories by volume; only the top six survive as their own band.
	perBucket := []map[string]int{
		{"a": 40, "b": 30, "c": 20, "d": 10, "e": 8, "f": 6, "g": 4, "h": 2},
		{"a": 10, "b": 10, "c": 10, "d": 10, "e": 10, "f": 10, "g": 10, "h": 10},
	}
	total := map[string]int{}
	for _, row := range perBucket {
		for k, v := range row {
			total[k] += v
		}
	}
	order, mix := foldCategoryMix(perBucket, total)

	// Six kept bands plus the folded "other", which sorts last.
	if len(order) != 7 || order[len(order)-1] != "other" {
		t.Fatalf("order = %v, want six categories then other", order)
	}
	for i, row := range mix {
		var sum float64
		for _, v := range row {
			sum += v
		}
		if math.Abs(sum-100) > 0.001 {
			t.Errorf("bucket %d sums to %.3f%%, want 100", i, sum)
		}
	}
	// The tail categories g and h folded into other, so they are not their own bands.
	for _, k := range order {
		if k == "g" || k == "h" {
			t.Errorf("tail category %q should have folded into other", k)
		}
	}
}

// churnFolder derives the treemap's middle drill level from a path: the directory for a
// nested file, "(root)" for a top-level one.
func TestChurnFolder(t *testing.T) {
	cases := map[string]string{
		"internal/server/store/analytics.go": "internal/server/store",
		"main.go":                            "(root)",
		"":                                   "(root)",
		"cmd/akari-server/main.go":           "cmd/akari-server",
	}
	for path, want := range cases {
		if got := churnFolder(path); got != want {
			t.Errorf("churnFolder(%q) = %q, want %q", path, got, want)
		}
	}
}
