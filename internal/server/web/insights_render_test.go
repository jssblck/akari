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
