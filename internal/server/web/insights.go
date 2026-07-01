package web

import (
	"fmt"
	"math"
	"strings"
	"time"

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
}

// distRows turns labeled counts into renderable bars: each width is its share of the
// largest count in the set, so the tallest bar is full and the rest are relative. A
// non-zero bucket always shows at least a sliver, so it never reads as empty next to a
// much larger neighbour. label and color map the canonical key to its display form.
func distRows(counts []store.LabeledCount, label, color func(string) string) []DistRow {
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
		rows = append(rows, DistRow{Label: label(c.Key), Count: c.Count, Pct: pct, Color: color(c.Key)})
	}
	return rows
}

// GradeBars renders the grade distribution: A through F then the unscored bucket, each
// banded in the report-card tone the session Quality tile uses.
func GradeBars(counts []store.LabeledCount) []DistRow {
	return distRows(counts, gradeLabel, gradeBarColor)
}

// OutcomeBars renders the outcome distribution, reusing OutcomeLabel for the title-cased
// names and a semantic tone per outcome.
func OutcomeBars(counts []store.LabeledCount) []DistRow {
	return distRows(counts, OutcomeLabel, outcomeBarColor)
}

// ArchetypeBars renders the archetype mix, lightest to heaviest, each in its own
// categorical hue.
func ArchetypeBars(counts []store.LabeledCount) []DistRow {
	return distRows(counts, titleCase, archetypeBarColor)
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
