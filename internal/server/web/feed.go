package web

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// The Sessions feed turns a flat list of session rows into a day-grouped, titled
// feed. The old surface was an eight-column grid where most columns repeated the
// same value down the page: one user, the same project in long runs, "today" on
// every row. This view leads each row with what the run was about, groups by day,
// and fades a repeated project, so the reader can pick out a half-remembered
// session instead of reading every figure. The grouping and titles are computed
// here so the template stays a dumb renderer.

// FeedRow is one session prepared for the feed: its source row plus the derived
// bits the template would otherwise have to compute inline.
type FeedRow struct {
	Row store.SessionRow
	// Title is the recognizable one-line label for the session, cleaned from its
	// opening prompt (Derived) or a fallback when no prompt exists (not Derived).
	Title   string
	Derived bool
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
func BuildSessionFeed(rows []store.SessionRow, grouped bool) []SessionDayGroup {
	return buildSessionFeed(time.Now(), rows, grouped)
}

// buildSessionFeed is BuildSessionFeed's clock-injected core, so the day headings
// are testable without mocking the wall clock.
func buildSessionFeed(now time.Time, rows []store.SessionRow, grouped bool) []SessionDayGroup {
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
		title, derived := SessionTitle(r)
		fr := FeedRow{Row: r, Title: title, Derived: derived, TokenPct: tokenPct(RowTokens(r.SessionSummary), maxTok)}

		if grouped {
			key, label := dayBucket(now, r.UpdatedAt)
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
// activity. The key is the UTC calendar date (so same-day rows share a group); the
// label reads relative for the recent past and falls back to a date. A missing
// timestamp lands in a single "Undated" group at the foot.
func dayBucket(now time.Time, t *time.Time) (key, label string) {
	if t == nil || t.IsZero() {
		return "", "Undated"
	}
	nu, tu := now.UTC(), t.UTC()
	nd := time.Date(nu.Year(), nu.Month(), nu.Day(), 0, 0, 0, 0, time.UTC)
	td := time.Date(tu.Year(), tu.Month(), tu.Day(), 0, 0, 0, 0, time.UTC)
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

// SessionTitle is the recognizable one-line label for a session in the feed: the
// cleaned opening prompt when there is one, else a quiet "<agent> session"
// fallback (Derived is false), which the template renders muted because it names
// no specific work. The project and branch still ride the row's meta line, so the
// fallback never leaves a row unidentifiable.
func SessionTitle(r store.SessionRow) (title string, derived bool) {
	if t := cleanFirstPrompt(r.FirstPrompt); t != "" {
		return t, true
	}
	// A session with no messages at all (an ingested run that streamed nothing, or
	// parsed to zero turns) is named for what it is, so the reader can tell it apart
	// from a real run whose opening turn merely carried no prompt text.
	if r.MessageCount == 0 {
		return "empty session", false
	}
	return r.Agent + " session", false
}

// firstPrompt cleaning. The opening user message in a real agent transcript is
// usually the bare instruction, but Claude Code wraps a slash command in
// <command-name>/<command-args> tags and prefixes a local command with a
// <local-command-caveat> block, and an injected message can lead with markdown.
// The cleaner peels those wrappers off so the title reads as the human's intent.
var (
	reBlockSysReminder = regexp.MustCompile(`(?is)<system-reminder>.*?</system-reminder>`)
	reBlockCaveat      = regexp.MustCompile(`(?is)<[a-z-]*caveat>.*?</[a-z-]*caveat>`)
	reCommandName      = regexp.MustCompile(`(?is)<command-name>\s*(.*?)\s*</command-name>`)
	reCommandArgs      = regexp.MustCompile(`(?is)<command-args>\s*(.*?)\s*</command-args>`)
	reAnyTag           = regexp.MustCompile(`(?s)<[^>]*>`)
	reLeadingMarkdown  = regexp.MustCompile("^[#>*`~\\-\\s]+")
	reWhitespace       = regexp.MustCompile(`\s+`)
)

// titleMax caps a derived title's length; only the opening line earns a place in
// the feed, so a long first prompt is cut on a word boundary with an ellipsis.
const titleMax = 96

func cleanFirstPrompt(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	s := reBlockSysReminder.ReplaceAllString(raw, " ")
	s = reBlockCaveat.ReplaceAllString(s, " ")

	// A slash command reads best as its name and arguments ("/impeccable init ..."),
	// reconstructed from the tags rather than shown as raw markup.
	if m := reCommandName.FindStringSubmatch(s); m != nil {
		name := strings.TrimSpace(m[1])
		args := ""
		if a := reCommandArgs.FindStringSubmatch(s); a != nil {
			args = strings.TrimSpace(a[1])
		}
		return truncateTitle(squish(strings.TrimSpace(name + " " + args)))
	}

	// Otherwise drop any remaining wrapper tags and a leading markdown run, then
	// collapse to a single line.
	s = reAnyTag.ReplaceAllString(s, " ")
	s = reLeadingMarkdown.ReplaceAllString(s, "")
	return truncateTitle(squish(s))
}

// squish collapses every whitespace run (including the newlines a multi-line
// prompt carries) to a single space and trims the ends, so a title is always one
// clean line.
func squish(s string) string {
	return strings.TrimSpace(reWhitespace.ReplaceAllString(s, " "))
}

// truncateTitle caps a title at titleMax runes, backing up to the last word break
// near the cap so it never slices a word, and trims trailing punctuation before
// the ellipsis.
func truncateTitle(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= titleMax {
		return s
	}
	cut := titleMax
	for i := titleMax; i > titleMax-24 && i > 0; i-- {
		if r[i] == ' ' {
			cut = i
			break
		}
	}
	return strings.TrimRight(string(r[:cut]), " ,.;:") + "…"
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

// titleClass styles a feed row's title: a derived title (from the prompt) reads in
// full text, a fallback ("<agent> session") reads muted because it names no
// specific work.
func titleClass(derived bool) string {
	if derived {
		return "srow-title"
	}
	return "srow-title fallback"
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

// FeedTime is the clock time a feed row shows. The day already rides the group
// heading, so the row needs only the time of day; the exact stamp is the cell's
// title on hover.
func FeedTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "--:--"
	}
	return t.UTC().Format("15:04")
}
