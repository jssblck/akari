package web

import (
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// dayBucket labels a session's last activity relative to a fixed clock and shares
// a key across same-day rows so the feed groups them together.
func TestDayBucket(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { tt := now.Add(d); return &tt }

	cases := []struct {
		name      string
		t         *time.Time
		wantLabel string
	}{
		{"today", at(-2 * time.Hour), "Today"},
		{"yesterday", at(-26 * time.Hour), "Yesterday"},
		{"within the week reads as a weekday", at(-3 * 24 * time.Hour), "Friday"},
		{"older this year reads as a date", at(-40 * 24 * time.Hour), "Wed May 20"},
		{"missing timestamp is undated", nil, "Undated"},
	}
	for _, c := range cases {
		if _, label := dayBucket(now, c.t); label != c.wantLabel {
			t.Errorf("%s: label = %q, want %q", c.name, label, c.wantLabel)
		}
	}

	// Two same-day rows share a grouping key.
	k1, _ := dayBucket(now, at(-1*time.Hour))
	k2, _ := dayBucket(now, at(-5*time.Hour))
	if k1 != k2 {
		t.Errorf("same-day rows should share a key, got %q and %q", k1, k2)
	}
}

// buildSessionFeed groups rows by day in the most-recent order, fades a repeated
// project, and scales each row's token bar against the feed's largest session.
func TestBuildSessionFeed(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	today1 := now.Add(-1 * time.Hour)
	today2 := now.Add(-3 * time.Hour)
	yesterday := now.Add(-26 * time.Hour)
	rows := []store.SessionRow{
		{SessionSummary: store.SessionSummary{ID: 1, Agent: "claude", TotalInput: 1000, UpdatedAt: &today1}, ProjectID: 1, ProjectKey: "akari", ProjectName: "akari", ProjectKind: "remote"},
		{SessionSummary: store.SessionSummary{ID: 2, Agent: "claude", TotalInput: 500, UpdatedAt: &today2}, ProjectID: 1, ProjectKey: "akari", ProjectName: "akari", ProjectKind: "remote"},
		{SessionSummary: store.SessionSummary{ID: 3, Agent: "claude", TotalInput: 0, UpdatedAt: &yesterday}, ProjectID: 2, ProjectKey: "site", ProjectName: "site", ProjectKind: "remote"},
	}

	groups := buildSessionFeed(now, rows, true)
	if len(groups) != 2 {
		t.Fatalf("want 2 day groups, got %d", len(groups))
	}
	if groups[0].Label != "Today" || len(groups[0].Rows) != 2 {
		t.Errorf("first group should be Today with 2 rows, got %q with %d", groups[0].Label, len(groups[0].Rows))
	}
	// The second Today row repeats the first row's project, so it fades; the first
	// does not.
	if groups[0].Rows[0].FadeProject {
		t.Error("the first row in a group should never fade its project")
	}
	if !groups[0].Rows[1].FadeProject {
		t.Error("a row repeating the project above it should fade")
	}
	// The bar uses a square-root scale against the feed maximum (1000), so the
	// 500-token row sits at sqrt(0.5) ~= 70%, not a linear 50%.
	if pct := groups[0].Rows[1].TokenPct; pct != 70 {
		t.Errorf("token bar should use a sqrt scale, got %d, want 70", pct)
	}
	// A zero-token row shows no bar.
	if pct := groups[1].Rows[0].TokenPct; pct != 0 {
		t.Errorf("a zero-token row should have no bar, got %d", pct)
	}

	// Ungrouped (any non-recent sort) yields a single, unlabeled group in order.
	flat := buildSessionFeed(now, rows, false)
	if len(flat) != 1 || flat[0].Label != "" || len(flat[0].Rows) != 3 {
		t.Errorf("ungrouped feed should be one unlabeled group of all rows, got %d groups", len(flat))
	}
}

// FeedTime renders the clock time of day, with a placeholder for a missing stamp.
func TestFeedTime(t *testing.T) {
	tt := time.Date(2026, 6, 29, 14, 22, 0, 0, time.UTC)
	if got := FeedTime(&tt); got != "14:22" {
		t.Errorf("FeedTime = %q, want 14:22", got)
	}
	if got := FeedTime(nil); got != "--:--" {
		t.Errorf("FeedTime(nil) = %q, want --:--", got)
	}
}
