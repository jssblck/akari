package web

import (
	"context"
	"fmt"
	"math"
	"strconv"
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
	// TokenPct is the row's total token volume as a percent of the feed's token
	// denominator (the largest session on the first loaded page, held stable across
	// keyset pages), backing a magnitude bar so the big runs stand out without the
	// reader parsing seven-digit figures. It is 0 for a zero-token session.
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
// whose project repeats the previous row's is flagged to mute its label. maxTok is
// the token-bar denominator every row scales against: the first page establishes it
// (FeedMaxTokens) and the keyset "Show more" carries it forward, so a bar's width
// means the same magnitude on page three as on page one rather than re-normalizing
// to each appended page's own maximum.
func BuildSessionFeed(ctx context.Context, rows []store.SessionRow, grouped bool, prevKey string, maxTok int64) []SessionDayGroup {
	return buildSessionFeed(time.Now(), Loc(ctx), rows, grouped, prevKey, maxTok)
}

// FeedMaxTokens is the token-bar denominator for a page of feed rows: the largest
// session's total token volume across the page. The first page computes it and the
// keyset "Show more" carries it forward (SessionFooter.MaxTok) so every appended page
// scales its bars against the same reference the reader already sees. Recomputing it
// per page would make a bar's width incomparable across a "Show more" boundary: a page
// of small sessions would render them full-width against their own small maximum. A
// later page holding a session larger than this denominator clamps to a full bar
// (tokenPct caps at 100), which reads as "the biggest so far" rather than misleading.
func FeedMaxTokens(rows []store.SessionRow) int64 {
	var maxTok int64
	for _, r := range rows {
		if t := RowTokens(r.SessionSummary); t > maxTok {
			maxTok = t
		}
	}
	return maxTok
}

// FeedDayKey is the day-bucket key of a timestamp in the viewer's zone, the same key
// buildSessionFeed groups on. The keyset "Show more" carries the last rendered row's key so
// the appended page can suppress a repeated day heading when it continues the same day (a
// page boundary must not print "Today" twice). It is empty for a missing timestamp, matching
// the undated bucket.
func FeedDayKey(ctx context.Context, t *time.Time) string {
	key, _ := dayBucket(time.Now(), Loc(ctx), t)
	return key
}

// buildSessionFeed is BuildSessionFeed's clock- and zone-injected core, so the day
// headings are testable without mocking the wall clock, and a session buckets under
// the viewer's local calendar date rather than UTC's. prevKey is the day-bucket key of
// the row immediately before this slice (the last row of the previous keyset page), or ""
// on the first page: when the first group's day matches it, that group's heading is dropped
// so an appended page continues the previous day rather than re-printing its heading.
func buildSessionFeed(now time.Time, loc *time.Location, rows []store.SessionRow, grouped bool, prevKey string, maxTok int64) []SessionDayGroup {
	if len(rows) == 0 {
		return nil
	}

	var groups []SessionDayGroup
	curKey := "" // the day bucket of the group being filled
	prevProj := ""
	started := false
	for _, r := range rows {
		fr := FeedRow{Row: r, TokenPct: tokenPct(RowTokens(r.SessionSummary), maxTok)}

		if grouped {
			key, label := dayBucket(now, loc, r.LastActiveAt)
			if !started || key != curKey {
				// Drop the heading on the very first group when it continues the day the
				// previous keyset page ended on, so an appended "Show more" page does not
				// reprint a heading ("Today") the DOM already shows above it.
				if !started && key == prevKey {
					label = ""
				}
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

// FilterOption is one choice in a toolbar filter select: the canonical key that rides
// the query string and its display label. Outcome and grade both use it, so the two
// selects render from one shape.
type FilterOption struct {
	Key   string
	Label string
}

// OutcomeFilterOptions are the outcome filter's choices, in the distribution's canonical
// order (best to worst, unknown last), so the toolbar select reads the same order the
// Insights outcome bars do. The labels reuse OutcomeLabel, so the select and the session
// detail's Outcome field name a bucket identically.
func OutcomeFilterOptions() []FilterOption {
	keys := []string{"completed", "errored", "abandoned", "unknown"}
	out := make([]FilterOption, 0, len(keys))
	for _, k := range keys {
		out = append(out, FilterOption{Key: k, Label: OutcomeLabel(k)})
	}
	return out
}

// GradeFilterOptions are the grade filter's choices: the five letters best to worst, then
// the "unscored" sentinel that matches a session with no gated grade (the Insights Grades
// panel's empty bucket). The letter labels are the letters themselves; the sentinel reuses
// gradeLabel so it reads "Unscored", the same word the grade bar carries.
func GradeFilterOptions() []FilterOption {
	keys := []string{"A", "B", "C", "D", "F", "unscored"}
	out := make([]FilterOption, 0, len(keys))
	for _, k := range keys {
		label := k
		if k == "unscored" {
			label = gradeLabel("")
		}
		out = append(out, FilterOption{Key: k, Label: label})
	}
	return out
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

// FanoutLabel is the fan-out chip's text: the subtree's subagent count and its
// whole-work-item cost, joined so a reader sees at a glance both how wide a prompt
// fanned out and what the whole thing cost. The count is singular at one ("1
// subagent"), and the cost carries the "+" lower-bound marker when any session in the
// subtree could not be fully priced. It is only ever rendered when the count is
// positive, so it never reads "0 subagents".
func FanoutLabel(tr store.TreeRollup) string {
	unit := "subagents"
	if tr.SubagentCount == 1 {
		unit = "subagent"
	}
	return fmt.Sprintf("%d %s · %s", tr.SubagentCount, unit, FmtCost(tr.CostUSD, tr.CostIncomplete))
}

// FanoutTitle is the fan-out chip's hover text, spelling out that the chip's cost is
// the whole work item's, not the root turn's. The row's own token cell shows the root
// session's own cost; a prompt that delegated to subagents spent far more than that
// root turn, and this line names that gap so the two cost figures do not read as a
// contradiction.
func FanoutTitle(tr store.TreeRollup) string {
	unit := "subagents"
	if tr.SubagentCount == 1 {
		unit = "subagent"
	}
	return fmt.Sprintf("Whole work item: %s across %d %s fanned out (the row's own cost is the root turn's alone)",
		FmtCost(tr.CostUSD, tr.CostIncomplete), tr.SubagentCount, unit)
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

// SessionFooter is the state the feed's footer renders under the list: the session
// count, whether a "Show more" button applies, and the empty-hidden toggle. It is
// computed once from the loaded rows and two bounded store probes (hasMore and
// hasEmpty) so the template stays a dumb renderer and the render cost stays linear in
// the page rather than the corpus: the old "N of M" carried a count(*) over the whole
// matching history, which the incremental-efficiency gate flagged.
type SessionFooter struct {
	// Shown is how many rows the feed renders cumulatively: on the first page it is
	// len(rows), and on each keyset "Show more" it is the prior total plus the appended
	// page, so the count grows as the reader loads deeper. When HasMore is false it is the
	// exact total (the whole set is loaded); when HasMore is true it reads "Showing N".
	Shown int
	// HasMore reports that at least one more row matches past the loaded pages (from
	// ListAllSessions' limit+1 probe), so the footer offers "Show more" and the count reads
	// "Showing N" rather than the exact "N sessions".
	HasMore bool
	// MoreHref is the "Show more" target: a keyset path carrying the last row's id as the
	// cursor (and, in the day-grouped order, its day key), set only when more rows match.
	// The button appends the next page onto the feed rather than re-rendering it, so depth
	// is unbounded; there is no cap.
	MoreHref string
	// MaxTok is the feed's token-bar denominator, the largest session's token volume on
	// the first loaded page. It rides the "Show more" cursor so each appended page scales
	// its bars against the same reference the first page set, keeping a bar's width
	// comparable across pages rather than re-normalizing to each page's own maximum.
	MaxTok int64
	// HasEmpty reports whether the current scope holds at least one empty (zero-message)
	// session, so the toggle appears only when it would change the feed; IncludeEmpty
	// reports whether those empties are being shown. Together they drive the terse
	// toggle: "empty hidden · show" when hidden, "showing empty · hide" when shown.
	HasEmpty     bool
	IncludeEmpty bool
	EmptyHref    string
}

// BuildSessionFooter assembles the footer state from the loaded page and two bounded probes:
// hasMore (does a further page exist, from the limit+1 read) and hasEmpty (does any empty
// session sit in scope). priorCount is the running total already shown before this page (0 on
// the first page, the cumulative count on a keyset append), so Shown reports the whole loaded
// feed. lastDayKey is the day-bucket key of the page's last row, carried into the "Show more"
// cursor so the next appended page can continue the same day without reprinting its heading;
// it is empty for a flat (non-grouped) order, where day headings do not apply. maxTok is the
// feed's token-bar denominator (FeedMaxTokens of the first page), carried into the "Show more"
// cursor so appended pages scale their bars against the same reference. The "Show more" link
// appears only when more rows match, and the empty toggle only when the scope holds an empty
// session (or already shows them, so the reader can hide them again).
func BuildSessionFooter(f store.SessionFilter, rows []store.SessionRow, priorCount int, hasMore, hasEmpty bool, lastDayKey string, maxTok int64) SessionFooter {
	ft := SessionFooter{
		Shown:        priorCount + len(rows),
		HasMore:      hasMore,
		HasEmpty:     hasEmpty,
		IncludeEmpty: f.IncludeEmpty,
		MaxTok:       maxTok,
	}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		ft.MoreHref = ShowMorePath(f, last.ID, keysetCursorValue(f, last), lastDayKey, ft.Shown, maxTok)
	}
	// The empty toggle is relevant only when hiding actually withholds something (or
	// when already showing empties, so the reader can hide them again). Either way a
	// scope that holds an empty session means the toggle would change the feed.
	if hasEmpty {
		ft.EmptyHref = string(EmptyToggleHref(f))
	}
	return ft
}

// keysetCursorValue formats the sort value of the feed's last visible row for the current
// order, so "Show more" can carry the boundary the page actually saw. The next page then
// resumes from that fixed value even if the cursor row's own column later moves (activity bumps
// last_active_at, a rebuild moves a count or cost), which would otherwise duplicate or skip rows
// (see store.SessionFilter.AfterVal and keysetCond). It returns "" for an order with no keyset
// cursor, where the value is unused. Each format is the exact, round-trippable text of the
// column's type: RFC3339 for the timestamp, plain integers for the counts, and the shortest
// float text for the cost (a double precision column, so the text casts back to the same
// float64). total_tokens sums the same four token classes migration 0014's generated column
// does, so the Go sum equals the value the query sorted by.
func keysetCursorValue(f store.SessionFilter, row store.SessionRow) string {
	switch effSort(f) {
	case store.DefaultSort: // "updated" -> last_active_at
		if row.LastActiveAt == nil {
			return ""
		}
		return row.LastActiveAt.UTC().Format(time.RFC3339Nano)
	case "tokens":
		return strconv.FormatInt(row.TotalInput+row.TotalOutput+row.TotalCacheRead+row.TotalCacheWrite, 10)
	case "messages":
		return strconv.Itoa(row.MessageCount)
	case "cost":
		return strconv.FormatFloat(row.TotalCostUSD, 'g', -1, 64)
	default:
		return ""
	}
}

// ValidKeysetValue reports whether val is a well-formed cursor value for the given feed sort
// key, so the handler can drop a tampered ?av (which would otherwise fail the SQL cast and 500)
// and fall back to the id-only cursor. It mirrors keysetCursorValue's per-column formats; an
// empty key reads as the default order, and a non-keyset order accepts no value.
func ValidKeysetValue(sortKey, val string) bool {
	if sortKey == "" {
		sortKey = store.DefaultSort
	}
	switch sortKey {
	case store.DefaultSort:
		_, err := time.Parse(time.RFC3339Nano, val)
		return err == nil
	case "tokens", "messages":
		_, err := strconv.ParseInt(val, 10, 64)
		return err == nil
	case "cost":
		_, err := strconv.ParseFloat(val, 64)
		return err == nil
	default:
		return false
	}
}

// HasEmptyToggle reports whether the footer shows the empty-hidden toggle.
func (ft SessionFooter) HasEmptyToggle() bool { return ft.EmptyHref != "" }

// CountLabel is the footer's session-count figure, tabular and terse. When the whole
// matching set fit in the page (no further page) the shown count IS the exact total,
// so it reads "N sessions"; when more match beyond the page it reads "Showing N",
// since the exact total is deliberately not counted.
func (ft SessionFooter) CountLabel() string {
	if ft.HasMore {
		return fmt.Sprintf("Showing %d", ft.Shown)
	}
	if ft.Shown == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", ft.Shown)
}

// EmptyToggleLabel is the terse toggle copy: "empty hidden" and a "show" verb when
// empties are hidden, or "showing empty" and a "hide" verb when they are shown. It
// returns the two parts (the state text and the verb) so the template can style the
// verb as the link affordance. The count is gone with the O(total) aggregate that
// produced it: the toggle only reports the state, not the magnitude.
func (ft SessionFooter) EmptyToggleLabel() (text, verb string) {
	if ft.IncludeEmpty {
		return "showing empty", "hide"
	}
	return "empty hidden", "show"
}
