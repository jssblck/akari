package web

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// FmtRate renders a per-minute throughput to one decimal, so the velocity figures read at a
// granularity that reads as a rate without implying false precision.
func FmtRate(v float64) string {
	return fmt.Sprintf("%.1f", v)
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
// path, and annotated with how many sessions returned to it.
type ChurnBar struct {
	// Project is the file's project display label, shown as a tag beside the path so the
	// churn list groups the same relative path per project across worktrees.
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
type DistRow struct {
	Label string
	Count int
	Pct   float64
	Color string
	// Href, when set, drills the row into the matching session list (a /sessions link
	// carrying the grade or outcome filter plus the current window). It is empty for a
	// dimension with no session-list filter (archetypes) and for a zero-count row, which
	// stays plain text rather than linking to an empty list.
	Href string
}

// distRows turns labeled counts into renderable bars: each width is its share of the
// largest count in the set, so the tallest bar is full and the rest are relative. A
// non-zero bucket always shows at least a sliver, so it never reads as empty next to a
// much larger neighbour. label and color map the canonical key to its display form. href
// builds the drill-through link for a bucket, or returns "" when the dimension is not
// filterable; a zero-count bucket never links regardless, since it would open an empty
// list.
func distRows(counts []store.LabeledCount, label, color func(string) string, href func(string) string) []DistRow {
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
		link := ""
		if href != nil && c.Count > 0 {
			link = href(c.Key)
		}
		rows = append(rows, DistRow{Label: label(c.Key), Count: c.Count, Pct: pct, Color: color(c.Key), Href: link})
	}
	return rows
}

// GradeBars renders the grade distribution: A through F then the unscored bucket, each
// banded in the report-card tone the session Quality tile uses. Each non-empty bar links
// into the matching sessions, carrying the current window (rng) so the session list is
// scoped to the same period the panel counted. base is the drill-down's starting filter:
// the fleet Insights page passes an empty filter (a fleet-wide drill), while the project
// page passes a filter already scoped to the project so the drill stays inside it. IncludeEmpty
// rides along because the panel counts sessions regardless of message_count (a zero-message
// session can still carry a grade), so the drilled feed must show empties too or its count
// would fall short of the bar it drilled from.
func GradeBars(counts []store.LabeledCount, base store.SessionFilter, rng string) []DistRow {
	return distRows(counts, gradeLabel, gradeBarColor, func(key string) string {
		f := base
		f.Grade = GradeFilterKey(key)
		f.Range = drillRange(rng)
		f.IncludeEmpty = true
		return SessionsPath(f)
	})
}

// OutcomeBars renders the outcome distribution, reusing OutcomeLabel for the title-cased
// names and a semantic tone per outcome. Each non-empty bar links into the matching
// sessions, carrying base (the project scope on the project page, empty on the fleet
// Insights page), the current window (rng), and IncludeEmpty for the same reason GradeBars
// does: the panel scope counts zero-message sessions, so the drilled feed must include them
// to match.
func OutcomeBars(counts []store.LabeledCount, base store.SessionFilter, rng string) []DistRow {
	return distRows(counts, OutcomeLabel, outcomeBarColor, func(key string) string {
		f := base
		f.Outcome = key
		f.Range = drillRange(rng)
		f.IncludeEmpty = true
		return SessionsPath(f)
	})
}

// ArchetypeBars renders the archetype mix, lightest to heaviest, each in its own
// categorical hue. Archetypes have no session-list filter, so their rows do not link.
func ArchetypeBars(counts []store.LabeledCount) []DistRow {
	return distRows(counts, titleCase, archetypeBarColor, nil)
}

// GradeBarsPlain and OutcomeBarsPlain are the drill-free counterparts to GradeBars
// and OutcomeBars, for the public project overview. Passing a nil href builder leaves
// every row's Href empty, so distributionPanel renders plain bars instead of links
// into the private session feed: the public page shows the quality distribution but
// keeps the sessions behind it private (they publish one at a time, not through the
// project's overview).
func GradeBarsPlain(counts []store.LabeledCount) []DistRow {
	return distRows(counts, gradeLabel, gradeBarColor, nil)
}

func OutcomeBarsPlain(counts []store.LabeledCount) []DistRow {
	return distRows(counts, OutcomeLabel, outcomeBarColor, nil)
}

// drillRange normalizes an Insights window key for a drill-through link. The "all" window
// applies no bound, so it is dropped rather than carried as a chip that would window
// nothing (the bare, unwindowed session list is what "all" means). Every other key rides
// through so the session list matches the panel's period.
func drillRange(rng string) string {
	if rng == "" || rng == "all" {
		return ""
	}
	return rng
}

// gradeLabel is the bar label for a grade key: the letter, or "Unscored" for the empty
// bucket (an unknown outcome with no tool signal, deliberately left ungraded).
func gradeLabel(key string) string {
	if key == "" {
		return "Unscored"
	}
	return key
}

// UnscoredKey is the sentinel a drill-through link and the Grade filter carry for the
// unscored grade bucket, since the empty string reads as "no grade filter". The Grades
// panel's unscored bar links with this value.
const UnscoredKey = "unscored"

// IsGrade reports whether v is a grade the session list can filter by: a letter A..F or
// the unscored sentinel. The handler uses it to reject a tampered ?grade= value.
func IsGrade(v string) bool {
	switch v {
	case "A", "B", "C", "D", "F", UnscoredKey:
		return true
	}
	return false
}

// IsOutcome reports whether v is a filterable outcome, so the handler can reject a
// tampered ?outcome= value.
func IsOutcome(v string) bool {
	switch v {
	case "completed", "abandoned", "errored", "unknown":
		return true
	}
	return false
}

// GradeFilterKey maps a Grades-distribution key to the ?grade= value that drills into the
// matching sessions: the empty (unscored) bucket becomes the sentinel, a letter stays
// itself. It is the inverse of gradeLabel for URL building.
func GradeFilterKey(distKey string) string {
	if distKey == "" {
		return UnscoredKey
	}
	return distKey
}

// GradeChipLabel and OutcomeChipLabel render the active-filter chip value for a grade or
// outcome, terse to match the agent/user chips ("grade A", "outcome abandoned").
func GradeChipLabel(grade string) string {
	if grade == UnscoredKey {
		return "unscored"
	}
	return grade
}

func OutcomeChipLabel(outcome string) string { return outcome }

// titleCase upper-cases the first rune of a lowercase key (an archetype name) for
// display, leaving the rest as is.
func titleCase(s string) string {
	if s == "" {
		return ""
	}
	return strings.ToUpper(s[:1]) + s[1:]
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

// GradedNote is the coverage caption for a quality distribution panel: the share of
// in-scope sessions that carry a usable grade, so a reader can weigh a mostly ungraded
// window's bars for what they are. It reads "" when the window is empty (the panel shows
// its own empty state), and rounds to whole percent, with a "<1% graded" floor so a
// nonzero-but-tiny coverage does not round away to "0% graded".
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

