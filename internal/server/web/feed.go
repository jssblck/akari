package web

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// The Sessions feed turns a flat list of session rows into a day-grouped feed. The
// old surface was an eight-column grid where most columns repeated the same value
// down the page: one user, the same project in long runs, "today" on every row.
// This view groups by day and fades a repeated project, so the reader can pick out
// a run from its metadata (project, branch, agent, size, time) without parsing
// every figure. The grouping is computed here so the template stays a dumb renderer.

// FeedRow is one session prepared for the feed: its source row plus the derived
// bits the template would otherwise have to compute inline.
type FeedRow struct {
	Row store.SessionRow
	// FadeProject marks a row whose project repeats the row directly above it in
	// the same day group, so the template can mute the repeated label and let a run
	// of same-project sessions read as one burst rather than a stuttering column.
	FadeProject bool
	// TokenPct is the row's total token volume as a percent of the feed's largest
	// session, backing a magnitude bar so the big runs stand out without the reader
	// parsing seven-digit figures. It is 0 for a zero-token session.
	TokenPct int
}

// SessionDayGroup is a run of feed rows under one day heading. Label is empty when
// the feed is not day-grouped (any order other than most-recent), in which case
// the template renders the rows without a heading.
type SessionDayGroup struct {
	Label string
	Rows  []FeedRow
}

// BuildSessionFeed groups session rows for the feed. When grouped is true (the
// most-recent order) the rows, already sorted newest first, are split under day
// headings ("Today", "Yesterday", a weekday, then a date); otherwise they form a
// single unlabeled group in the order the query returned. Within a group a row
// whose project repeats the previous row's is flagged to mute its label. Token
// bars scale against the largest session across the whole feed, so magnitudes are
// comparable between groups.
func BuildSessionFeed(ctx context.Context, rows []store.SessionRow, grouped bool) []SessionDayGroup {
	return buildSessionFeed(time.Now(), Loc(ctx), rows, grouped)
}

// buildSessionFeed is BuildSessionFeed's clock- and zone-injected core, so the day
// headings are testable without mocking the wall clock, and a session buckets under
// the viewer's local calendar date rather than UTC's.
func buildSessionFeed(now time.Time, loc *time.Location, rows []store.SessionRow, grouped bool) []SessionDayGroup {
	if len(rows) == 0 {
		return nil
	}
	var maxTok int64
	for _, r := range rows {
		if t := RowTokens(r.SessionSummary); t > maxTok {
			maxTok = t
		}
	}

	var groups []SessionDayGroup
	curKey := "" // the day bucket of the group being filled
	prevProj := ""
	started := false
	for _, r := range rows {
		fr := FeedRow{Row: r, TokenPct: tokenPct(RowTokens(r.SessionSummary), maxTok)}

		if grouped {
			key, label := dayBucket(now, loc, r.UpdatedAt)
			if !started || key != curKey {
				groups = append(groups, SessionDayGroup{Label: label})
				curKey = key
				prevProj = ""
				started = true
			}
		} else if !started {
			groups = append(groups, SessionDayGroup{})
			started = true
		}

		proj := SessionRowProject(r)
		fr.FadeProject = proj == prevProj
		prevProj = proj

		last := len(groups) - 1
		groups[last].Rows = append(groups[last].Rows, fr)
	}
	return groups
}

// tokenPct scales a row's tokens to a 0..100 bar width against the feed maximum.
// Token volume across sessions is heavily skewed (one long run can hold ten times
// the tokens of a typical one), so a linear bar would peg the outlier full and
// floor every other row into the same sliver. A square-root scale spreads the
// mid-range, so more rows differentiate while the largest session still reads
// fullest; the exact figure and breakdown stay one hover away. Any nonzero volume
// keeps a thin floor so a small-but-real session still shows a mark.
func tokenPct(tok, max int64) int {
	if max <= 0 || tok <= 0 {
		return 0
	}
	pct := int(math.Sqrt(float64(tok)/float64(max)) * 100)
	if pct < 3 {
		return 3
	}
	if pct > 100 {
		return 100
	}
	return pct
}

// dayBucket returns a stable grouping key and a display label for a session's last
// activity. The key is the calendar date in the viewer's zone (so rows that share a
// local day share a group, and a run does not split across the reader's midnight);
// the label reads relative for the recent past and falls back to a date. A missing
// timestamp lands in a single "Undated" group at the foot.
func dayBucket(now time.Time, loc *time.Location, t *time.Time) (key, label string) {
	if t == nil || t.IsZero() {
		return "", "Undated"
	}
	nu, tu := now.In(loc), t.In(loc)
	nd := time.Date(nu.Year(), nu.Month(), nu.Day(), 0, 0, 0, 0, loc)
	td := time.Date(tu.Year(), tu.Month(), tu.Day(), 0, 0, 0, 0, loc)
	key = td.Format("2006-01-02")
	days := int(nd.Sub(td).Hours() / 24)
	switch {
	case days <= 0:
		return key, "Today"
	case days == 1:
		return key, "Yesterday"
	case days < 7:
		return key, tu.Format("Monday")
	case nd.Year() == td.Year():
		return key, tu.Format("Mon Jan 2")
	default:
		return key, tu.Format("Jan 2, 2006")
	}
}

// SessionSortOption is one choice in the feed's sort control. The cross-project
// feed sorts by recency, volume, or message count; the categorical dimensions
// (agent, project, machine, user) are served by the filters instead, so the
// control stays a short list rather than a header per column.
type SessionSortOption struct {
	Key   string
	Label string
}

// SessionSortOptions are the feed's sort choices, in menu order. "updated" (the
// default) is the only one that day-groups the feed; the others read as a flat,
// ranked list.
func SessionSortOptions() []SessionSortOption {
	return []SessionSortOption{
		{Key: "updated", Label: "Recent"},
		{Key: "tokens", Label: "Most tokens"},
		{Key: "messages", Label: "Most messages"},
		{Key: "cost", Label: "Most expensive"},
	}
}

// FeedIsGrouped reports whether the feed for this filter is split into day
// headings, which it is only in the default most-recent order.
func FeedIsGrouped(f store.SessionFilter) bool {
	return effSort(f) == store.DefaultSort
}

// sortSelected marks the sort control's current choice.
func sortSelected(f store.SessionFilter, key string) bool {
	return effSort(f) == key
}

// projClass mutes a project label that repeats the row above it, so a run of
// same-project sessions reads as one burst rather than a stuttering column.
func projClass(fade bool) string {
	if fade {
		return "srow-proj faded"
	}
	return "srow-proj"
}

// barStyle is the inline width for a token magnitude bar. The bar is static (no
// grow animation): a hundred bars settling at once would be motion for its own
// sake, which the feed avoids.
func barStyle(pct int) string {
	return fmt.Sprintf("width:%d%%", pct)
}

// FeedTime is the clock time a feed row shows, in the viewer's timezone. The day
// already rides the group heading, so the row needs only the time of day; the exact
// stamp is the cell's title on hover.
func FeedTime(ctx context.Context, t *time.Time) string {
	if t == nil || t.IsZero() {
		return "--:--"
	}
	return t.In(Loc(ctx)).Format("15:04")
}

// SnippetParts splits a search snippet into its before/match/after text runs so the
// template can render each as an auto-escaped text node and wrap only the middle in
// <mark>. The <mark> element is thus template structure around plain text: the
// matched content is never interpolated as markup, so a query or a message that
// contains "<script>" renders as escaped text inside the highlight rather than
// injecting an element. Offsets out of range (a malformed snippet) collapse to the
// whole text as the "before" run with an empty match, so a bad window degrades to
// plain unhighlighted text rather than a panic.
type SnippetParts struct {
	Before string
	Match  string
	After  string
}

// SnippetParts computes the three runs from a store snippet's byte offsets.
func SplitSnippet(s store.SearchSnippet) SnippetParts {
	t := s.Text
	if s.MatchStart < 0 || s.MatchEnd > len(t) || s.MatchStart > s.MatchEnd {
		return SnippetParts{Before: t}
	}
	return SnippetParts{
		Before: t[:s.MatchStart],
		Match:  t[s.MatchStart:s.MatchEnd],
		After:  t[s.MatchEnd:],
	}
}

// SessionFooter is the state the feed's footer renders under the list: the "N of M"
// count, whether a "Show more" button applies, and the empty-hidden toggle. It is
// computed once from the loaded rows and the count query so the template stays a
// dumb renderer and the two never disagree about what the numbers mean.
type SessionFooter struct {
	// Shown is how many rows the feed currently renders; Total is how many match the
	// filter across the whole corpus (from CountAllSessions).
	Shown int
	Total int
	// MoreHref is the "Show more" target (a doubled-limit path), set only when more
	// rows match than are shown and the page is below the cap.
	MoreHref string
	// AtCap reports the feed is showing the maximum page (500) yet more match, so the
	// footer names the cap and drops the button, asking the reader to narrow instead.
	AtCap bool
	// EmptyHidden is how many empty (zero-message) sessions the current scope holds;
	// IncludeEmpty reports whether they are being shown. Together they drive the
	// terse empty toggle: "K empty hidden · show" when hidden, "showing empty · hide"
	// when shown. The toggle is omitted when no empty session exists in scope.
	EmptyHidden  int
	IncludeEmpty bool
	EmptyHref    string
}

// BuildSessionFooter assembles the footer state from the loaded rows, the filter,
// and the count query's total and empty-in-scope figures. shown is len(rows); the
// "Show more" button appears only when shown < total and the page is below the cap,
// and the empty toggle appears only when the scope actually holds an empty session.
func BuildSessionFooter(f store.SessionFilter, shown, total, emptyHidden int) SessionFooter {
	ft := SessionFooter{
		Shown:        shown,
		Total:        total,
		EmptyHidden:  emptyHidden,
		IncludeEmpty: f.IncludeEmpty,
	}
	limit := effLimit(f)
	switch {
	case shown < total && limit >= MaxSessionLimit:
		// At the cap with more matching: name the cap, no button, narrow instead.
		ft.AtCap = true
	case shown < total:
		ft.MoreHref = ShowMorePath(f)
	}
	// The empty toggle is relevant only when hiding actually withholds something (or
	// when already showing empties, so the reader can hide them again). When empties
	// are shown, emptyHidden counts how many empty rows are in the shown set; when
	// hidden, it counts how many are being withheld. Either way a nonzero value means
	// the toggle would change the feed.
	if emptyHidden > 0 {
		ft.EmptyHref = string(EmptyToggleHref(f))
	}
	return ft
}

// HasEmptyToggle reports whether the footer shows the empty-hidden toggle.
func (ft SessionFooter) HasEmptyToggle() bool { return ft.EmptyHref != "" }

// CountLabel is the footer's "N of M" figure, tabular and terse.
func (ft SessionFooter) CountLabel() string {
	return fmt.Sprintf("%d of %d", ft.Shown, ft.Total)
}

// EmptyToggleLabel is the terse toggle copy: the count of hidden empties and a
// "show" verb when they are hidden, or "showing empty" and a "hide" verb when they
// are shown. It returns the two parts (the count/state text and the verb) so the
// template can style the verb as the link affordance.
func (ft SessionFooter) EmptyToggleLabel() (text, verb string) {
	if ft.IncludeEmpty {
		return "showing empty", "hide"
	}
	return fmt.Sprintf("%d empty hidden", ft.EmptyHidden), "show"
}
