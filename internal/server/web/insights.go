package web

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/jssblck/akari/internal/server/store"
)

// ConcurrencyBusiest formats the busiest user's peak for the concurrency panel: the name
// and their peak simultaneous sessions, or a dash when no user had a measurable span.
func ConcurrencyBusiest(c store.ConcurrencyStats) string {
	if c.BusiestUser == "" {
		return "-"
	}
	return fmt.Sprintf("%s (%d)", c.BusiestUser, c.BusiestUserPeak)
}

// FmtAvgConcurrent renders the average concurrency to one decimal, the granularity that
// reads as a rate ("1.4 concurrent") without implying false precision.
func FmtAvgConcurrent(v float64) string {
	return fmt.Sprintf("%.1f", v)
}

// FmtRate renders a per-minute throughput to one decimal, the same granularity as the
// concurrency average so the velocity and concurrency figures read in one register.
func FmtRate(v float64) string {
	return fmt.Sprintf("%.1f", v)
}

// VelocityMsgsRate and VelocityToolsRate format the throughput figures, dashing them when
// the scope had no active time to divide by (see VelocityStats.HasThroughput): a 0.0 over
// an undefined denominator would read as a real "no work happened" rate rather than "no
// rate to show".
func VelocityMsgsRate(v store.VelocityStats) string {
	if !v.HasThroughput() {
		return "-"
	}
	return FmtRate(v.MsgsPerActiveMin)
}

func VelocityToolsRate(v store.VelocityStats) string {
	if !v.HasThroughput() {
		return "-"
	}
	return FmtRate(v.ToolsPerActiveMin)
}

// FmtLatency renders a turn-cycle latency at the coarsest unit that still reads honestly:
// seconds under a minute, minutes and seconds under an hour, hours and minutes beyond. A
// zero or negative duration (no measured turn) shows a dash rather than "0s", so an absent
// figure never reads as an instantaneous reply. The value rounds to whole seconds before
// the unit split, so 59.6s reads as "1m 0s" rather than a misleading "60s".
func FmtLatency(d time.Duration) string {
	secs := d.Seconds()
	if secs <= 0 {
		return "-"
	}
	if secs < 1 {
		return "<1s"
	}
	whole := int(math.Round(secs))
	switch {
	case whole < 60:
		return fmt.Sprintf("%ds", whole)
	case whole < 3600:
		return fmt.Sprintf("%dm %ds", whole/60, whole%60)
	default:
		return fmt.Sprintf("%dh %dm", whole/3600, (whole%3600)/60)
	}
}

// FmtErrorRate renders a failure share (a 0..1 fraction) as a whole-percent figure, with
// a "<1%" floor so a small but real error rate never rounds down to a reassuring "0%".
func FmtErrorRate(v float64) string {
	pct := v * 100
	if pct > 0 && pct < 1 {
		return "<1%"
	}
	return fmt.Sprintf("%.0f%%", pct)
}

// HygienePct renders a prompt-hygiene rate (a count over the prompt or session total) as
// a whole-percent figure. It keeps the "<1%" floor the tool error rate uses, so a rare
// but present signal does not round away to a clean 0%. A zero count reads as a real 0%
// (no such prompts), not a dash; the dash is only the guard for an empty denominator,
// which the panel already avoids by gating on PromptHygiene.HasData.
func HygienePct(n, d int) string {
	if d <= 0 {
		return "-"
	}
	pct := float64(n) / float64(d) * 100
	if pct > 0 && pct < 1 {
		return "<1%"
	}
	return fmt.Sprintf("%.0f%%", pct)
}

// HygieneCount renders the raw count behind a hygiene rate ("12 of 340"), the sub-line
// under each figure, so a reader sees the magnitude and not only the proportion (3% of
// 1000 prompts and 3% of 30 read very differently).
func HygieneCount(n, d int) string {
	return fmt.Sprintf("%d of %d", n, d)
}

// ToolBar is one tool's bar in the mix: sized by call volume, coloured by its error band,
// and annotated with its error rate when it had any failures. The dual encoding reads mix
// (bar length) and reliability (colour and the error suffix) in one row.
type ToolBar struct {
	Name    string
	Calls   int
	Pct     float64
	ErrText string // "" when the tool never failed, else its error rate ("12%")
	Color   string
}

// ToolBars turns the fleet's busiest tools into renderable bars: each width is its share
// of the most-called tool, so the busiest tool is full and the rest are relative, and each
// colour bands the tool's reliability. A non-zero bar always shows at least a sliver so a
// rarely-called tool never reads as absent beside a dominant one.
func ToolBars(t store.ToolStats) []ToolBar {
	var maxCalls int
	for _, x := range t.Tools {
		if x.Calls > maxCalls {
			maxCalls = x.Calls
		}
	}
	bars := make([]ToolBar, 0, len(t.Tools))
	for _, x := range t.Tools {
		pct := 0.0
		if maxCalls > 0 {
			pct = float64(x.Calls) / float64(maxCalls) * 100
		}
		if pct > 0 && pct < 2 {
			pct = 2
		}
		errText := ""
		if x.Failures > 0 {
			errText = FmtErrorRate(x.ErrorRate())
		}
		bars = append(bars, ToolBar{Name: x.Name, Calls: x.Calls, Pct: pct, ErrText: errText, Color: toolBarColor(x.ErrorRate())})
	}
	return bars
}

// toolBarColor bands a tool's error rate into the report-card tones: clean tools read sage,
// a moderate failure share peach, and a heavy one rose, so an unreliable tool stands out in
// the mix without reading its exact rate.
func toolBarColor(errRate float64) string {
	switch {
	case errRate <= 0:
		return barSage
	case errRate < 0.15:
		return barPeach
	default:
		return barRose
	}
}

// ChurnBar is one file's bar in the churn list: sized by edit count, labelled with its
// (worktree-invariant) path and its owning project, and annotated with how many sessions
// returned to it. Project is carried so the panel can prefix each bar with its project, since
// the same relative path can churn in more than one project and the two must read apart.
type ChurnBar struct {
	Project  string
	Path     string
	Edits    int
	Sessions int
	Pct      float64
}

// ChurnBars turns the most-edited files into renderable bars, each width its share of the
// most-churned file so the worst hotspot reads full and the rest relative.
func ChurnBars(c store.FileChurn) []ChurnBar {
	var maxEdits int
	for _, f := range c.Files {
		if f.Edits > maxEdits {
			maxEdits = f.Edits
		}
	}
	bars := make([]ChurnBar, 0, len(c.Files))
	for _, f := range c.Files {
		pct := 0.0
		if maxEdits > 0 {
			pct = float64(f.Edits) / float64(maxEdits) * 100
		}
		if pct > 0 && pct < 2 {
			pct = 2
		}
		bars = append(bars, ChurnBar{Project: f.Project, Path: f.Path, Edits: f.Edits, Sessions: f.Sessions, Pct: pct})
	}
	return bars
}

// ChurnSessions labels how many sessions edited a file, so a path churned across the fleet
// reads apart from one a single session kept rewriting.
func ChurnSessions(sessions int) string {
	if sessions == 1 {
		return "1 session"
	}
	return fmt.Sprintf("%d sessions", sessions)
}

// Distribution bar colours, drawn from the data-viz ramp and the status palette so the
// Insights bars read in the same hues as the rest of the app. Grades and outcomes carry
// a semantic tone (good / watch / poor); archetypes carry a categorical sequence. The
// muted tone is for the catch-all buckets (unscored, unknown) that carry no verdict.
const (
	barSage  = "#a6d29e" // good
	barPeach = "#f0bf92" // watch
	barRose  = "#ec98b0" // poor
	barMuted = "#74747e" // no verdict
)

// DistRow is one bar in an Insights distribution: a display label, the session count,
// the fill width as a percent of the largest bar in the set, and the bar colour. It
// reuses the breakdown bars' markup and animation, so a distribution reads with the
// same instrument styling as the cost breakdowns.
//
// Href, when non-empty, makes the whole row a drill-down link into the sessions feed
// filtered to this bucket, so a team lead can go from a distribution bar to the sessions
// behind it. Title is the link's terse hover label ("View completed sessions"). The
// archetype bars leave both empty (no archetype session filter exists), so they render as
// plain rows.
type DistRow struct {
	Label string
	Count int
	Pct   float64
	Color string
	Href  string
	Title string
}

// distRows turns labeled counts into renderable bars: each width is its share of the
// largest count in the set, so the tallest bar is full and the rest are relative. A
// non-zero bucket always shows at least a sliver, so it never reads as empty next to a
// much larger neighbour. label and color map the canonical key to its display form; href
// maps it to a drill-down URL (and hover title), or returns two empty strings for a bucket
// that does not drill in.
func distRows(counts []store.LabeledCount, label, color func(string) string, href func(key, label string) (string, string)) []DistRow {
	var maxN int
	for _, c := range counts {
		if c.Count > maxN {
			maxN = c.Count
		}
	}
	rows := make([]DistRow, 0, len(counts))
	for _, c := range counts {
		pct := 0.0
		if maxN > 0 {
			pct = float64(c.Count) / float64(maxN) * 100
		}
		if pct > 0 && pct < 2 {
			pct = 2
		}
		lbl := label(c.Key)
		row := DistRow{Label: lbl, Count: c.Count, Pct: pct, Color: color(c.Key)}
		if href != nil {
			row.Href, row.Title = href(c.Key, lbl)
		}
		rows = append(rows, row)
	}
	return rows
}

// GradeBars renders the grade distribution: A through F then the unscored bucket, each
// banded in the report-card tone the session Quality tile uses. Each bar drills into the
// sessions feed filtered to that grade (the unscored bucket to ?grade=unscored, matching
// the store's sentinel), so a reader can go from a bar to the sessions behind it.
//
// base is the scope the drill-down should carry alongside the grade: the fleet-wide
// /insights page passes the zero filter (a bar links to ?grade=A over the whole fleet),
// while the project page passes a filter bearing the project id and any active
// user/agent/machine narrowing, so the link honours the page's current scope (landing on
// ?project=<id>&grade=A rather than the unscoped feed). The bucket field overlays a copy of
// base, so base's own Grade is never read (a grade bar always sets its own).
//
// rng is the analytics window the bar counts describe (the active ?range key). It rides the
// drill-down href so the feed the bar opens is bounded to the same window the bar counted, rather
// than the count showing a window's sessions while the link opens the all-time feed. "all" and the
// empty key add no bound (the feed's natural all-history form).
func GradeBars(counts []store.LabeledCount, base store.SessionFilter, rng string) []DistRow {
	return distRows(counts, gradeLabel, gradeBarColor, gradeHref(base, rng))
}

// OutcomeBars renders the outcome distribution, reusing OutcomeLabel for the title-cased
// names and a semantic tone per outcome. Each bar drills into the feed filtered to that
// outcome, scoped by base and windowed by rng the same way GradeBars is (see its note).
func OutcomeBars(counts []store.LabeledCount, base store.SessionFilter, rng string) []DistRow {
	return distRows(counts, OutcomeLabel, outcomeBarColor, outcomeHref(base, rng))
}

// ArchetypeBars renders the archetype mix, lightest to heaviest, each in its own
// categorical hue. It passes no href builder: there is no archetype session filter, so the
// bars stay plain rows rather than dangling a link that would land on the unfiltered feed.
func ArchetypeBars(counts []store.LabeledCount) []DistRow {
	return distRows(counts, titleCase, archetypeBarColor, nil)
}

// outcomeHref builds an outcome bar's drill-down link from a base scope: it overlays the
// bucket's canonical key (the same value the toolbar's outcome select carries) onto a copy
// of base, so a bar's count and the feed it opens describe the same sessions under the same
// scope. Returning a closure lets the page thread its project (and any active
// user/agent/machine narrowing) into every bar without the distRows plumbing knowing about
// scope.
func outcomeHref(base store.SessionFilter, rng string) func(key, label string) (string, string) {
	return func(key, label string) (string, string) {
		f := base
		f.Outcome = key
		return SessionsPath(f, rng), "View " + strings.ToLower(label) + " sessions"
	}
}

// gradeHref builds a grade bar's drill-down link from a base scope. A letter filters on that
// grade; the empty bucket filters on the "unscored" sentinel, matching the store's coalesce
// so the bar and its drill-down list agree. Like outcomeHref it overlays the grade onto a
// copy of base, so the link carries the page's project and filter scope.
func gradeHref(base store.SessionFilter, rng string) func(key, label string) (string, string) {
	return func(key, label string) (string, string) {
		grade := key
		title := "View grade " + label + " sessions"
		if key == "" {
			grade = "unscored"
			title = "View unscored sessions"
		}
		f := base
		f.Grade = grade
		return SessionsPath(f, rng), title
	}
}

// gradeLabel is the bar label for a grade key: the letter, or "Unscored" for the empty
// bucket (an unknown outcome with no tool signal, deliberately left ungraded).
func gradeLabel(key string) string {
	if key == "" {
		return "Unscored"
	}
	return key
}

// titleCase upper-cases the first rune of a lowercase key (an archetype name) for
// display, leaving the rest as is.
func titleCase(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// GradedNote is the Grades panel's coverage note: the whole-percent share of the cohort
// that carries a gated grade ("62% graded"), so a reader weighs the distribution by how
// much of the window it actually speaks for. It is empty for an empty cohort (nothing to
// take a share of) and floors a small-but-real share at "<1%", the same register
// FmtErrorRate and HygienePct use, so a scope where only a handful graded never reads as a
// reassuring 0%.
func GradedNote(q store.QualityDistribution) string {
	if q.Sessions == 0 {
		return ""
	}
	pct := float64(q.Graded) / float64(q.Sessions) * 100
	if pct > 0 && pct < 1 {
		return "<1% graded"
	}
	return fmt.Sprintf("%.0f%% graded", pct)
}

// UserQualityHref links a People-panel row to the sessions feed scoped to that author, the same
// ?user param the toolbar's user select carries, so a click on a name lands on their sessions.
// rng carries the active analytics window onto the link, so the feed opens bounded to the same
// trailing window the row's counts describe rather than the author's all-time sessions.
func UserQualityHref(u store.UserQuality, rng string) templ.SafeURL {
	return SessionsHref(store.SessionFilter{Username: u.Username}, rng)
}

// UserGradedLabel renders the graded-coverage cell as "N of M", the raw magnitude behind the
// share, so a row reads how many of an author's sessions carry a grade without the reader
// dividing in their head.
func UserGradedLabel(u store.UserQuality) string {
	return fmt.Sprintf("%d of %d", u.Graded, u.Sessions)
}

// UserAvgScore renders an author's average quality score to one decimal, or a dash when no
// session in scope is scored (AvgScore nil), so an unmeasured author reads as an abstention
// rather than a zero that would look like a real (failing) average.
func UserAvgScore(u store.UserQuality) string {
	if u.AvgScore == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *u.AvgScore)
}

// OutcomeSegment is one span of a People row's stacked outcome bar: a colour, a width as a
// percent of the row's sessions, and the raw count (0 spans are dropped by OutcomeSegments,
// so a rendered segment always has a count). The four segments together span the row's full
// width, since the outcome counts partition the session total.
type OutcomeSegment struct {
	Color string
	Pct   float64
	Count int
	Label string
}

// OutcomeSegments splits an author's sessions into the stacked-bar segments the People panel
// draws: completed, abandoned, errored, then unknown, each sized as its share of the row's
// sessions and coloured by the same semantic tones the outcome distribution uses. A zero
// bucket is dropped so the bar carries no empty span. The four counts partition Sessions, so
// the kept segments always sum to the full width; an author with no sessions yields none.
func OutcomeSegments(u store.UserQuality) []OutcomeSegment {
	if u.Sessions <= 0 {
		return nil
	}
	raw := []OutcomeSegment{
		{Color: barSage, Count: u.Completed, Label: "completed"},
		{Color: barPeach, Count: u.Abandoned, Label: "abandoned"},
		{Color: barRose, Count: u.Errored, Label: "errored"},
		{Color: barMuted, Count: u.Unknown, Label: "unknown"},
	}
	out := make([]OutcomeSegment, 0, len(raw))
	for _, s := range raw {
		if s.Count <= 0 {
			continue
		}
		s.Pct = float64(s.Count) / float64(u.Sessions) * 100
		out = append(out, s)
	}
	return out
}

// OutcomeMixTitle is the hover label for a People row's outcome bar, spelling out the counts
// the coloured spans encode ("12 completed, 3 abandoned, 1 errored, 4 unknown"), so the exact
// split is one hover away from the at-a-glance proportion. Every bucket is listed, including
// zeros, so the four always read in the same order.
func OutcomeMixTitle(u store.UserQuality) string {
	return fmt.Sprintf("%d completed, %d abandoned, %d errored, %d unknown",
		u.Completed, u.Abandoned, u.Errored, u.Unknown)
}

// SegmentStyle is the inline width and colour for one outcome-bar segment. The colour is set
// inline rather than through the animateBars data-color path (which only claims
// .bar-fill[data-pct]) because these segments are static: a table of rows settling at once
// would be motion for its own sake, the same rationale the feed's magnitude bars apply, so
// they carry their colour directly instead of waiting on the grow handler.
func SegmentStyle(seg OutcomeSegment) string {
	return fmt.Sprintf("width:%.2f%%;background:%s", seg.Pct, seg.Color)
}

func gradeBarColor(grade string) string {
	switch grade {
	case "A", "B":
		return barSage
	case "C":
		return barPeach
	case "D", "F":
		return barRose
	default: // unscored
		return barMuted
	}
}

func outcomeBarColor(outcome string) string {
	switch outcome {
	case "completed":
		return barSage
	case "abandoned":
		return barPeach
	case "errored":
		return barRose
	default: // unknown
		return barMuted
	}
}

func archetypeBarColor(a string) string {
	switch a {
	case "quick":
		return "#95c0ef" // sky
	case "standard":
		return "#88cfce" // teal
	case "deep":
		return "#ddc885" // gold
	case "marathon":
		return barRose
	default: // automation
		return barMuted
	}
}
