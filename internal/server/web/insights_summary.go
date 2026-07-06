package web

import (
	"fmt"
	"math"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
)

// InsightsSummary is the plain-language read at the top of Insights: the window's quality,
// spend, and tool reliability said in a sentence or three, so a lead gets the shape of the
// window before scanning a single instrument. It is the synthesis the tiles below cannot
// give on their own, pairing the waste share with the spend it eats and the failure rate
// with the call volume behind it.
//
// Each sentence is emitted only when its figure is actually measured: a scope with no graded
// session drops the quality line rather than claiming a GPA of zero, and a window with no
// priced spend drops the spend line rather than printing "$0". So the strip never states a
// number the page cannot stand behind, and an empty slice means nothing was measurable (the
// page's own empty-state gate already covers a window with no sessions at all). The sentences
// come back in reading order for the strip to join.
func InsightsSummary(ins store.Insights) []string {
	var out []string

	// Quality: the session-weighted GPA over the graded cohort, reported with its coverage so
	// a GPA resting on two graded sessions out of two hundred reads as thin, not settled.
	if q := ins.Quality; q.Graded > 0 {
		var points float64
		for _, gc := range q.Grades {
			points += quality.GPAPoints(gc.Key) * float64(gc.Count)
		}
		gpa := points / float64(q.Graded)
		out = append(out, fmt.Sprintf("Graded %d of %d sessions at GPA %.2f.", q.Graded, q.Sessions, gpa))
	}

	// Spend: the window total with the abandoned share pulled out, the same waste framing the
	// Overview verdict leads with, so the two surfaces name the leak in the same words.
	if ins.Trends != nil {
		if e := ins.Trends.Economics; e.TotalSpend > 0 {
			s := fmt.Sprintf("Spend totaled %s", FmtCost(e.TotalSpend, e.CostIncomplete))
			// Name the abandoned share only when it rounds to a whole percent or more. A
			// window can abandon a few cents out of hundreds of dollars, which rounds to 0%;
			// "with 0% ($0.12) sunk" reads as broken, and a sub-percent leak is not the thing
			// a summary should call out. Below the threshold the spend line stands alone.
			if pct := int(math.Round(e.AbandonedSharePct)); pct >= 1 {
				s += fmt.Sprintf(", with %d%% (%s) sunk into abandoned sessions",
					pct, FmtCost(e.TotalAbandoned, e.AbandonedIncomplete))
			}
			out = append(out, s+".")
		}
	}

	// Tools: the failure rate over the window's tool calls, the reliability read the Tools
	// instrument details bucket by bucket, given here as one headline number.
	if t := ins.Tools; t.TotalCalls > 0 {
		rate := float64(t.TotalFailures) / float64(t.TotalCalls) * 100
		out = append(out, fmt.Sprintf("Tools failed on %.1f%% of %s calls.", rate, FmtTokens(int64(t.TotalCalls))))
	}

	return out
}
