package web

import (
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

func sampleInsights() store.Insights {
	return store.Insights{
		Concurrency: store.ConcurrencyStats{
			FleetPeak:       4,
			FleetPeakAt:     time.Date(2026, 6, 12, 14, 30, 0, 0, time.UTC),
			BusiestUser:     "ada",
			BusiestUserPeak: 3,
			AvgConcurrent:   1.7,
			Sessions:        15,
		},
		Velocity: store.VelocityStats{
			ResponseP50:       25 * time.Second,
			ResponseP90:       90 * time.Second,
			FirstResponseP50:  8 * time.Second,
			MsgsPerActiveMin:  4.2,
			ToolsPerActiveMin: 2.5,
			ActiveSeconds:     600,
			Turns:             12,
			Sessions:          15,
		},
		Tools: store.ToolStats{
			TotalCalls:    120,
			TotalFailures: 12,
			Turns:         40,
			Tools: []store.ToolStat{
				{Name: "Read", Calls: 60, Failures: 0},
				{Name: "Edit", Calls: 30, Failures: 4},
				{Name: "Bash", Calls: 20, Failures: 8},
				{Name: "Grep", Calls: 10, Failures: 0},
			},
			Clipped: 2,
		},
		Churn: store.FileChurn{
			Files: []store.ChurnFile{
				{Path: "internal/server/store/analytics.go", Edits: 6, Sessions: 2},
				{Path: "app.js", Edits: 3, Sessions: 1},
			},
			Clipped: 1,
		},
		Quality: store.QualityDistribution{
			Grades: []store.LabeledCount{
				{Key: "A", Count: 5}, {Key: "B", Count: 3}, {Key: "C", Count: 2},
				{Key: "D", Count: 1}, {Key: "F", Count: 0}, {Key: "", Count: 4},
			},
			Outcomes: []store.LabeledCount{
				{Key: "completed", Count: 8}, {Key: "errored", Count: 2},
				{Key: "abandoned", Count: 1}, {Key: "unknown", Count: 4},
			},
			Sessions: 15,
		},
		Archetypes: []store.LabeledCount{
			{Key: "quick", Count: 6}, {Key: "standard", Count: 5}, {Key: "deep", Count: 2},
			{Key: "marathon", Count: 1}, {Key: "automation", Count: 1},
		},
	}
}

// The Insights page renders the three distribution panels with their bars, the unscored
// grade bucket labeled rather than blank, the in-window session count, and the
// window selector wired to swap the section.
func TestInsightsPageRendersDistributions(t *testing.T) {
	p := Page{Title: "Insights", LoggedIn: true, Active: "insights", Username: "ada"}
	ranges := RangeOptions("/insights", nil, "30d")
	html := renderComponent(t, InsightsPage(p, sampleInsights(), "30d", ranges))

	for _, want := range []string{
		`id="insights"`,            // the swap target
		`>Concurrency<`,            // the headline band
		`>Grades<`, `>Outcomes<`, `>Archetypes<`, // the three distribution panels
		`15 sessions in window`,    // the summary count
		`>4</div>`,                 // the fleet peak figure
		`>peak at once<`,           // its label
		`ada (3)`,                  // the busiest user and their peak
		`>1.7</div>`,               // average concurrent, one decimal
		`>Velocity<`,               // the other headline band
		`>25s</div>`,               // response p50, formatted seconds
		`>response p50<`,           // its label
		`>1m 30s</div>`,            // response p90, formatted minutes and seconds
		`>4.2</div>`,               // messages per active minute, one decimal
		`>msgs / active min<`,      // its label
		`>Tools<`,                  // the tools band
		`>120</div>`,               // total tool calls
		`>tool calls<`,             // its label
		`>10%</div>`,               // fleet error rate (12/120)
		`>tools / turn<`,           // the tools-per-turn label
		`>Read<`,                   // the busiest tool in the mix
		`class="tool-err"`,         // a failing tool carries its error rate
		`>40%<`,                    // Bash's error rate suffix (8/20)
		`+2 more tools not shown`,  // the clipped-tail note
		`>File churn<`,             // the churn panel
		`internal/server/store/analytics.go`, // the churned path (full path in the label)
		`6 edits`,                  // its edit count
		`2 sessions`,               // spread across two sessions
		`+1 more churned file not shown`, // the churn clip note
		`>Unscored<`,               // the empty grade key reads as a word, not a blank
		`>Completed<`,              // outcome label, title-cased
		`>Quick<`,                  // archetype label, title-cased
		`class="bar-fill"`,         // reuses the breakdown bar markup
		`data-color="` + barSage + `"`, // a graded bar carries its tone
		`hx-get="/insights?range=7d"`,  // the window selector refetches this page
		`hx-select="#insights"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("insights page missing %q", want)
		}
	}
}

// When the window has graded sessions but none carry a measured start and end, the
// distribution grid still renders while the concurrency band falls back to a note
// instead of showing a real-looking row of zeros.
func TestInsightsPageConcurrencyNoSpans(t *testing.T) {
	p := Page{Title: "Insights", LoggedIn: true, Active: "insights", Username: "ada"}
	ranges := RangeOptions("/insights", nil, "30d")
	ins := sampleInsights()
	ins.Concurrency = store.ConcurrencyStats{} // no measured spans

	html := renderComponent(t, InsightsPage(p, ins, "30d", ranges))

	if !strings.Contains(html, "No sessions with a measured start and end in this window.") {
		t.Error("concurrency band should note the missing spans")
	}
	if strings.Contains(html, `class="conc-figs"`) {
		t.Error("concurrency band should not render figures when there are no spans")
	}
	if !strings.Contains(html, `>Grades<`) {
		t.Error("the distribution grid should still render alongside the concurrency note")
	}
}

// When the window has sessions but none carry a timed turn, the velocity band falls back
// to a note instead of a row of dashes and zeros that would read as a real measurement.
func TestInsightsPageVelocityNoTurns(t *testing.T) {
	p := Page{Title: "Insights", LoggedIn: true, Active: "insights", Username: "ada"}
	ranges := RangeOptions("/insights", nil, "30d")
	ins := sampleInsights()
	ins.Velocity = store.VelocityStats{} // no measured turns

	html := renderComponent(t, InsightsPage(p, ins, "30d", ranges))

	if !strings.Contains(html, "No timed turns in this window.") {
		t.Error("velocity band should note the missing turns")
	}
	if !strings.Contains(html, `>Velocity<`) {
		t.Error("the velocity band heading should still render")
	}
}

// When the window has sessions but none ran a tool, the tools band shows a note instead of
// an empty bar list, while the rest of the page renders as usual.
func TestInsightsPageToolsEmpty(t *testing.T) {
	p := Page{Title: "Insights", LoggedIn: true, Active: "insights", Username: "ada"}
	ranges := RangeOptions("/insights", nil, "30d")
	ins := sampleInsights()
	ins.Tools = store.ToolStats{} // no tool calls

	html := renderComponent(t, InsightsPage(p, ins, "30d", ranges))

	if !strings.Contains(html, "No tool calls in this window.") {
		t.Error("tools band should note the missing tool calls")
	}
	if !strings.Contains(html, `>Tools<`) {
		t.Error("the tools band heading should still render")
	}
}

// When the window edited no file more than once, the churn panel shows a note instead of
// an empty bar list.
func TestInsightsPageChurnEmpty(t *testing.T) {
	p := Page{Title: "Insights", LoggedIn: true, Active: "insights", Username: "ada"}
	ranges := RangeOptions("/insights", nil, "30d")
	ins := sampleInsights()
	ins.Churn = store.FileChurn{} // nothing edited twice

	html := renderComponent(t, InsightsPage(p, ins, "30d", ranges))

	if !strings.Contains(html, "No files were edited more than once in this window.") {
		t.Error("churn panel should note the absence of repeated edits")
	}
	if !strings.Contains(html, `>File churn<`) {
		t.Error("the churn panel heading should still render")
	}
}

// With no sessions in the window the page shows an empty state instead of a row of
// zero-height bars.
func TestInsightsPageEmptyState(t *testing.T) {
	p := Page{Title: "Insights", LoggedIn: true, Active: "insights", Username: "ada"}
	ranges := RangeOptions("/insights", nil, "7d")
	empty := store.Insights{} // Quality.Sessions == 0
	html := renderComponent(t, InsightsPage(p, empty, "7d", ranges))

	if !strings.Contains(html, "No sessions in this window yet.") {
		t.Error("empty insights page should show the empty state")
	}
	if strings.Contains(html, `class="ins-grid"`) {
		t.Error("empty insights page should not render the distribution grid")
	}
}
