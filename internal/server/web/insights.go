package web

import (
	"strings"

	"github.com/jssblck/akari/internal/server/store"
)

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
