package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// TestKeysetCursorValue pins the "Show more" cursor value the footer carries for each feed
// order, and that ShowMorePath and the handler's validation agree on its form: a value produced
// for an order validates for that order, garbage does not, and the value rides the URL as ?av.
func TestKeysetCursorValue(t *testing.T) {
	at := time.Date(2026, 5, 1, 8, 30, 0, 0, time.UTC)
	var row store.SessionRow
	row.LastActiveAt = &at
	// total_tokens is input+output+cache_read+cache_write (migration 0014): 100+200+300+400.
	row.TotalInput, row.TotalOutput, row.TotalCacheRead, row.TotalCacheWrite = 100, 200, 300, 400
	row.MessageCount = 12
	row.TotalCostUSD = 3.5

	cases := []struct{ sort, want string }{
		{"", at.Format(time.RFC3339Nano)}, // default order -> last_active_at
		{"updated", at.Format(time.RFC3339Nano)},
		{"tokens", "1000"},
		{"messages", "12"},
		{"cost", "3.5"},
		{"project", ""}, // a non-keyset order carries no cursor value
	}
	for _, c := range cases {
		f := store.SessionFilter{Sort: c.sort}
		got := keysetCursorValue(f, row)
		if got != c.want {
			t.Errorf("keysetCursorValue(sort=%q) = %q, want %q", c.sort, got, c.want)
		}
		if c.want == "" {
			continue
		}
		if !ValidKeysetValue(c.sort, c.want) {
			t.Errorf("ValidKeysetValue(%q, %q) = false, want true (own value must validate)", c.sort, c.want)
		}
		if ValidKeysetValue(c.sort, "not-a-cursor-value") {
			t.Errorf("ValidKeysetValue(%q, garbage) = true, want false", c.sort)
		}
	}

	// The value rides the "Show more" URL as ?av, and is omitted when empty (a non-keyset order).
	withVal := ShowMorePath(store.SessionFilter{}, 42, "2026-05-01T08:30:00Z", "", 0, 0)
	if !strings.Contains(withVal, "after=42") || !strings.Contains(withVal, "av=") {
		t.Errorf("ShowMorePath should carry after and av, got %q", withVal)
	}
	if noVal := ShowMorePath(store.SessionFilter{}, 42, "", "", 0, 0); strings.Contains(noVal, "av=") {
		t.Errorf("ShowMorePath should omit av when empty, got %q", noVal)
	}
}

// dayBucket labels a session's last activity relative to a fixed clock and shares
// a key across same-day rows so the feed groups them together. The calendar date is
// the viewer's, so the same instant can land in a different bucket by zone.
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
		if _, label := dayBucket(now, time.UTC, c.t); label != c.wantLabel {
			t.Errorf("%s: label = %q, want %q", c.name, label, c.wantLabel)
		}
	}

	// Two same-day rows share a grouping key.
	k1, _ := dayBucket(now, time.UTC, at(-1*time.Hour))
	k2, _ := dayBucket(now, time.UTC, at(-5*time.Hour))
	if k1 != k2 {
		t.Errorf("same-day rows should share a key, got %q and %q", k1, k2)
	}

	// The bucket follows the viewer's calendar date. At 2026-06-29 07:30 UTC a stamp
	// from 2026-06-29 05:00 UTC is "Today" in UTC, but a viewer in US Pacific (PDT,
	// eight hours behind) reads it as 2026-06-28 22:00 local, the previous day, so it
	// buckets under "Yesterday" with a distinct grouping key.
	pacific, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("America/Los_Angeles unavailable: %v", err)
	}
	nowUTC := time.Date(2026, 6, 29, 7, 30, 0, 0, time.UTC)
	stamp := time.Date(2026, 6, 29, 5, 0, 0, 0, time.UTC)
	utcKey, utcLabel := dayBucket(nowUTC, time.UTC, &stamp)
	laKey, laLabel := dayBucket(nowUTC, pacific, &stamp)
	if utcLabel != "Today" {
		t.Errorf("UTC label = %q, want Today", utcLabel)
	}
	if laLabel != "Yesterday" {
		t.Errorf("Pacific label = %q, want Yesterday", laLabel)
	}
	if utcKey == laKey {
		t.Errorf("zones disagreeing on the calendar date should yield distinct keys, both = %q", utcKey)
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
		{SessionSummary: store.SessionSummary{ID: 1, Agent: "claude", TotalInput: 1000, LastActiveAt: &today1}, ProjectID: 1, ProjectKey: "akari", ProjectName: "akari", ProjectKind: "remote"},
		{SessionSummary: store.SessionSummary{ID: 2, Agent: "claude", TotalInput: 500, LastActiveAt: &today2}, ProjectID: 1, ProjectKey: "akari", ProjectName: "akari", ProjectKind: "remote"},
		{SessionSummary: store.SessionSummary{ID: 3, Agent: "claude", TotalInput: 0, LastActiveAt: &yesterday}, ProjectID: 2, ProjectKey: "site", ProjectName: "site", ProjectKind: "remote"},
	}

	groups := buildSessionFeed(now, time.UTC, rows, true, "", FeedMaxTokens(rows))
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
	flat := buildSessionFeed(now, time.UTC, rows, false, "", FeedMaxTokens(rows))
	if len(flat) != 1 || flat[0].Label != "" || len(flat[0].Rows) != 3 {
		t.Errorf("ungrouped feed should be one unlabeled group of all rows, got %d groups", len(flat))
	}
}

// TestFeedTokenDenominatorCarries pins the fix for token bars rescaling across a keyset
// "Show more": buildSessionFeed scales every row against the maxTok it is handed, not the
// page's own maximum, so an appended page of small sessions renders thin bars against the
// denominator the first page established rather than pegging them full and reading larger
// than the bigger sessions already on screen.
func TestFeedTokenDenominatorCarries(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	at := now.Add(-1 * time.Hour)
	// An appended page whose own largest session is 250 tokens, small next to the bigger
	// sessions the reader already loaded (a first-page denominator of 1000).
	small := []store.SessionRow{
		{SessionSummary: store.SessionSummary{ID: 10, Agent: "claude", TotalInput: 250, LastActiveAt: &at}, ProjectID: 1, ProjectKey: "akari", ProjectName: "akari", ProjectKind: "remote"},
	}
	// Against the carried denominator (1000) the 250-token row is sqrt(0.25) = 50%.
	if pct := buildSessionFeed(now, time.UTC, small, true, "", 1000)[0].Rows[0].TokenPct; pct != 50 {
		t.Errorf("a row scaled against the carried denominator should read 50, got %d", pct)
	}
	// Sanity: recomputed per page (the old behavior) the same row pegs full against its own
	// 250 max, which is exactly the cross-page miscalibration the carried denominator fixes.
	if pct := buildSessionFeed(now, time.UTC, small, true, "", FeedMaxTokens(small))[0].Rows[0].TokenPct; pct != 100 {
		t.Errorf("against its own page max the row pegs full, got %d", pct)
	}
	// A session larger than the carried denominator clamps to a full bar rather than
	// overflowing it, reading as "the biggest so far".
	big := []store.SessionRow{
		{SessionSummary: store.SessionSummary{ID: 11, Agent: "claude", TotalInput: 5000, LastActiveAt: &at}, ProjectID: 1, ProjectKey: "akari", ProjectName: "akari", ProjectKind: "remote"},
	}
	if pct := buildSessionFeed(now, time.UTC, big, true, "", 1000)[0].Rows[0].TokenPct; pct != 100 {
		t.Errorf("a row above the carried denominator should clamp to 100, got %d", pct)
	}
}

// TestBuildSessionFeedContinuesDay pins the keyset "Show more" continuation: when a
// prevKey names the day the previous page ended on, the first group of the appended page
// repeats that day and so drops its heading, rather than reprinting "Today" above rows the
// DOM already shows under it. A first page (empty prevKey) always keeps its heading, and a
// prevKey naming a different day still opens a fresh heading.
func TestBuildSessionFeedContinuesDay(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	today := now.Add(-1 * time.Hour)
	rows := []store.SessionRow{
		{SessionSummary: store.SessionSummary{ID: 5, Agent: "claude", LastActiveAt: &today}, ProjectID: 1, ProjectKey: "akari", ProjectName: "akari", ProjectKind: "remote"},
	}
	todayKey, _ := dayBucket(now, time.UTC, &today)

	// A continuation of the same day: the first (only) group repeats today, so its heading
	// is suppressed.
	cont := buildSessionFeed(now, time.UTC, rows, true, todayKey, FeedMaxTokens(rows))
	if len(cont) != 1 || cont[0].Label != "" {
		t.Errorf("a same-day continuation should drop the leading heading, got %d groups with label %q", len(cont), labelOf(cont))
	}
	// The rows still render; only the heading is dropped.
	if len(cont[0].Rows) != 1 {
		t.Errorf("the continuation should still carry its row, got %d", len(cont[0].Rows))
	}

	// A fresh first page (no prevKey) keeps the heading.
	first := buildSessionFeed(now, time.UTC, rows, true, "", FeedMaxTokens(rows))
	if first[0].Label != "Today" {
		t.Errorf("a first page should keep its Today heading, got %q", first[0].Label)
	}

	// A prevKey naming a different day opens a new heading, not a continuation.
	other := buildSessionFeed(now, time.UTC, rows, true, "2020-01-01", FeedMaxTokens(rows))
	if other[0].Label != "Today" {
		t.Errorf("a prevKey from another day should still head the group, got %q", other[0].Label)
	}
}

// labelOf reads the first group's label for a diagnostic, tolerating an empty feed.
func labelOf(groups []SessionDayGroup) string {
	if len(groups) == 0 {
		return "<none>"
	}
	return groups[0].Label
}

// TestOutcomeFilterOptions pins the outcome toolbar select's option set: the canonical
// best-to-worst order the distribution bars use, each key paired with OutcomeLabel's
// display text, and that FilterOption.Key round-trips as the value a handler would
// receive back from the select.
func TestOutcomeFilterOptions(t *testing.T) {
	opts := OutcomeFilterOptions()
	wantKeys := []string{"completed", "errored", "abandoned", "unknown"}
	if len(opts) != len(wantKeys) {
		t.Fatalf("got %d options, want %d", len(opts), len(wantKeys))
	}
	for i, want := range wantKeys {
		if opts[i].Key != want {
			t.Errorf("option[%d].Key = %q, want %q", i, opts[i].Key, want)
		}
		if opts[i].Label != OutcomeLabel(want) {
			t.Errorf("option[%d].Label = %q, want OutcomeLabel(%q) = %q", i, opts[i].Label, want, OutcomeLabel(want))
		}
		// The key is exactly what a selected option would submit back on the query
		// string, so a round-trip through IsOutcome (the handler's whitelist) must accept
		// it.
		if !IsOutcome(opts[i].Key) {
			t.Errorf("option key %q should round-trip through IsOutcome", opts[i].Key)
		}
	}
}

// TestGradeFilterOptions pins the grade toolbar select's option set: the five letters
// best to worst, then the unscored sentinel last, each label matching the letter itself
// (or "Unscored" for the sentinel), and every key round-tripping through IsGrade.
func TestGradeFilterOptions(t *testing.T) {
	opts := GradeFilterOptions()
	wantKeys := []string{"A", "B", "C", "D", "F", "unscored"}
	if len(opts) != len(wantKeys) {
		t.Fatalf("got %d options, want %d", len(opts), len(wantKeys))
	}
	for i, want := range wantKeys {
		if opts[i].Key != want {
			t.Errorf("option[%d].Key = %q, want %q", i, opts[i].Key, want)
		}
		if !IsGrade(opts[i].Key) {
			t.Errorf("option key %q should round-trip through IsGrade", opts[i].Key)
		}
	}
	wantLabels := []string{"A", "B", "C", "D", "F", "Unscored"}
	for i, want := range wantLabels {
		if opts[i].Label != want {
			t.Errorf("option[%d].Label = %q, want %q", i, opts[i].Label, want)
		}
	}
}

// TestFanoutLabel pins the fan-out chip's text: the subagent count with a singular unit
// at one, joined to the whole-work-item cost, and the "+" lower-bound marker riding an
// incomplete subtree cost.
func TestFanoutLabel(t *testing.T) {
	cases := []struct {
		name string
		tr   store.TreeRollup
		want string
	}{
		{"plural", store.TreeRollup{SubagentCount: 62, CostUSD: 4.12}, "62 subagents · $4.12"},
		{"singular", store.TreeRollup{SubagentCount: 1, CostUSD: 0.30}, "1 subagent · $0.30"},
		{"incomplete cost carries the plus marker", store.TreeRollup{SubagentCount: 3, CostUSD: 2.00, CostIncomplete: true}, "3 subagents · $2.00+"},
	}
	for _, c := range cases {
		if got := FanoutLabel(c.tr); got != c.want {
			t.Errorf("%s: FanoutLabel = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestFanoutTitle checks the hover text names the cost as the whole work item's and uses
// the same singular/plural unit as the chip, so the two never disagree on grammar.
func TestFanoutTitle(t *testing.T) {
	if got := FanoutTitle(store.TreeRollup{SubagentCount: 1, CostUSD: 0.30}); got !=
		"Whole work item: $0.30 across 1 subagent fanned out (the row's own cost is the root turn's alone)" {
		t.Errorf("FanoutTitle singular = %q", got)
	}
	if got := FanoutTitle(store.TreeRollup{SubagentCount: 4, CostUSD: 9.00, CostIncomplete: true}); got !=
		"Whole work item: $9.00+ across 4 subagents fanned out (the row's own cost is the root turn's alone)" {
		t.Errorf("FanoutTitle plural incomplete = %q", got)
	}
}

// FeedTime renders the clock time of day in the viewer's zone, with a placeholder
// for a missing stamp.
func TestFeedTime(t *testing.T) {
	tt := time.Date(2026, 6, 29, 14, 22, 0, 0, time.UTC)
	if got := FeedTime(context.Background(), &tt); got != "14:22" {
		t.Errorf("FeedTime = %q, want 14:22", got)
	}
	if got := FeedTime(context.Background(), nil); got != "--:--" {
		t.Errorf("FeedTime(nil) = %q, want --:--", got)
	}
	pacific, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("America/Los_Angeles unavailable: %v", err)
	}
	if got := FeedTime(WithLoc(context.Background(), pacific), &tt); got != "07:22" {
		t.Errorf("FeedTime Pacific = %q, want 07:22", got)
	}
}
