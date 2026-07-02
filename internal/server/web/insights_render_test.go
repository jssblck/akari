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
		Hygiene: store.PromptHygiene{
			Prompts:            200,
			Short:              20,
			Duplicate:          6,
			NoCodeContext:      14,
			Sessions:           15,
			UnstructuredStarts: 3,
		},
		Churn: store.FileChurn{
			Files: []store.ChurnFile{
				{Path: "internal/server/store/analytics.go", Edits: 6, Sessions: 2},
				{Path: "app.js", Edits: 3, Sessions: 1},
			},
			Clipped: 1,
		},
		Context: store.ContextHealthStats{
			Sessions:          15,
			PeakTokensP50:     128000,
			PeakTokensP90:     412000,
			PeakTokensMax:     1_200_000,
			TotalResets:       9,
			SessionsWithReset: 6,
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
			Graded:   11, // 15 total minus the 4 unscored; GradedNote reads 73%
		},
		Archetypes: []store.LabeledCount{
			{Key: "quick", Count: 6}, {Key: "standard", Count: 5}, {Key: "deep", Count: 2},
			{Key: "marathon", Count: 1}, {Key: "automation", Count: 1},
		},
		Users: store.UserQualityStats{
			Users: []store.UserQuality{
				{Username: "ada", Sessions: 9, Graded: 7, Completed: 6, Abandoned: 1, Errored: 1, Unknown: 1, AvgScore: f64(82.5)},
				{Username: "grace", Sessions: 6, Graded: 4, Completed: 3, Abandoned: 0, Errored: 1, Unknown: 2, AvgScore: nil},
			},
			Clipped: 1,
		},
	}
}

// f64 boxes a float for the nullable AvgScore fixtures.
func f64(v float64) *float64 { return &v }

// The Insights page renders the three distribution panels with their bars, the unscored
// grade bucket labeled rather than blank, the in-window session count, and the
// window selector wired to swap the section.
func TestInsightsPageRendersDistributions(t *testing.T) {
	p := Page{Title: "Insights", LoggedIn: true, Active: "insights", Username: "ada"}
	ranges := RangeOptions("/insights", nil, "30d")
	html := renderComponent(t, InsightsPage(p, sampleInsights(), "30d", ranges))

	for _, want := range []string{
		`id="insights"`,                          // the swap target
		`>Concurrency<`,                          // the headline band
		`>Grades<`, `>Outcomes<`, `>Archetypes<`, // the three distribution panels
		`title="A to F; unscored means the session was never graded"`, // the Grades definition moved to a tooltip
		`15 sessions in window`,              // the summary count
		`>4</div>`,                           // the fleet peak figure
		`>peak at once<`,                     // its label
		`ada (3)`,                            // the busiest user and their peak
		`>1.7</div>`,                         // average concurrent, one decimal
		`>Velocity<`,                         // the other headline band
		`>25s</div>`,                         // response p50, formatted seconds
		`>response p50<`,                     // its label
		`>1m 30s</div>`,                      // response p90, formatted minutes and seconds
		`>4.2</div>`,                         // messages per active minute, one decimal
		`>msgs / active min<`,                // its label
		`>Tools<`,                            // the tools band
		`>120</div>`,                         // total tool calls
		`>tool calls<`,                       // its label
		`>10%</div>`,                         // fleet error rate (12/120)
		`>tools / turn<`,                     // the tools-per-turn label
		`>Read<`,                             // the busiest tool in the mix
		`class="tool-err"`,                   // a failing tool carries its error rate
		`>40%<`,                              // Bash's error rate suffix (8/20)
		`+2 more tools not shown`,            // the clipped-tail note
		`>Prompt hygiene<`,                   // the input-quality band
		`>terse prompts<`,                    // a hygiene figure label
		`>repeated prompts<`,                 // another hygiene figure label
		`>no code pointer<`,                  // the no-code-context figure label
		`>unstructured start<`,               // the per-session opener figure label
		`20 of 200`,                          // the terse-prompt sub-count (unique to hygiene)
		`3 of 15`,                            // the unstructured-start sub-count (over sessions, not prompts)
		`>Context health<`,                   // the context-load band
		`>median peak<`,                      // a context figure label
		`>128.0k</div>`,                      // the median peak, compact tokens
		`>p90 peak<`,                         // the p90 figure label
		`>1.2M</div>`,                        // the heaviest peak, compact tokens
		`>shed context<`,                     // the reset-rate figure label
		`6 of 15 sessions`,                   // the shed-context sub-count (sessions that reset)
		`>context resets<`,                   // the total-resets figure label
		`Load, not spend.`,                   // the peak definition now lives on the figure label's title tooltip
		`>File churn<`,                       // the churn panel
		`internal/server/store/analytics.go`, // the churned path (full path in the label)
		`6 edits`,                            // its edit count
		`2 sessions`,                         // spread across two sessions
		`+1 more churned file not shown`,     // the churn clip note
		`>Unscored<`,                         // the empty grade key reads as a word, not a blank
		`>Completed<`,                        // outcome label, title-cased
		`>Quick<`,                            // archetype label, title-cased
		`class="bar-fill"`,                   // reuses the breakdown bar markup
		`data-color="` + barSage + `"`,       // a graded bar carries its tone
		`73% graded`,                         // the Grades panel coverage note (11 of 15)
		`class="bar-link"`,                   // a distribution bar renders as a drill-down link (the whole-row link)
		// The drill-downs carry the active window (range=30d) and empty=1 so a bar's count and the
		// feed it opens describe the same trailing window and the same empty-session policy the panel
		// counted under, rather than the bar counting 30 days while the link opens the all-time feed.
		`href="/sessions?empty=1&amp;outcome=completed&amp;range=30d"`, // the outcome bar drills into the windowed feed
		`href="/sessions?empty=1&amp;grade=A&amp;range=30d"`,           // a grade bar drills into its letter, windowed
		`href="/sessions?empty=1&amp;grade=unscored&amp;range=30d"`,    // the unscored bucket uses the sentinel
		`>People<`, // the per-user quality panel
		`href="/sessions?empty=1&amp;range=30d&amp;user=ada"`, // a username drills into that author's windowed sessions
		`class="mix-bar"`, // the per-row stacked outcome bar
		`6 completed, 1 abandoned, 1 errored, 1 unknown`, // the mix hover title spells out the counts
		`7 of 9`,                      // ada's graded coverage
		`>82.5<`,                      // ada's average score, one decimal
		`+1 more user not shown`,      // the clipped-tail note
		`hx-get="/insights?range=7d"`, // the window selector refetches this page
		`hx-select="#insights"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("insights page missing %q", want)
		}
	}
}

// The People panel hides on a single-author instance (its one row would only restate the
// fleet distributions under a name) and dashes an author whose average score is unmeasured.
func TestInsightsPagePeoplePanel(t *testing.T) {
	p := Page{Title: "Insights", LoggedIn: true, Active: "insights", Username: "ada"}
	ranges := RangeOptions("/insights", nil, "30d")

	// Two authors: the panel renders, and Grace's nil AvgScore dashes rather than reading 0.
	two := renderComponent(t, InsightsPage(p, sampleInsights(), "30d", ranges))
	if !strings.Contains(two, `>People<`) {
		t.Error("people panel should render for two authors")
	}
	// Grace's Avg score cell is a dash (nil AvgScore), in the last column of her row. Her link
	// carries the active window (range=30d) and empty=1 so it opens her windowed sessions under
	// the same empty-session policy the panel counted them under.
	if !strings.Contains(two, `href="/sessions?empty=1&amp;range=30d&amp;user=grace"`) {
		t.Error("grace's row should link to her windowed sessions")
	}

	// One author: the panel hides entirely.
	ins := sampleInsights()
	ins.Users = store.UserQualityStats{Users: []store.UserQuality{
		{Username: "ada", Sessions: 9, Graded: 7, Completed: 9, AvgScore: f64(82.5)},
	}}
	one := renderComponent(t, InsightsPage(p, ins, "30d", ranges))
	if strings.Contains(one, `>People<`) {
		t.Error("people panel should hide for a single author")
	}
	if strings.Contains(one, `id="ins-people"`) {
		t.Error("people panel wrapper should not render for a single author")
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

// When the window has sessions but none carry hygiene data, the prompt-hygiene band shows
// a note instead of a row of zero-over-zero rates, while the rest of the page renders.
func TestInsightsPageHygieneEmpty(t *testing.T) {
	p := Page{Title: "Insights", LoggedIn: true, Active: "insights", Username: "ada"}
	ranges := RangeOptions("/insights", nil, "30d")
	ins := sampleInsights()
	ins.Hygiene = store.PromptHygiene{} // no signalled prompts

	html := renderComponent(t, InsightsPage(p, ins, "30d", ranges))

	if !strings.Contains(html, "No prompts in this window.") {
		t.Error("hygiene band should note the absence of prompts")
	}
	if !strings.Contains(html, `>Prompt hygiene<`) {
		t.Error("the hygiene band heading should still render")
	}
}

// When the window has sessions but none carry measured context, the context-health band
// shows a note instead of a row of zeroes, while the rest of the page renders.
func TestInsightsPageContextEmpty(t *testing.T) {
	p := Page{Title: "Insights", LoggedIn: true, Active: "insights", Username: "ada"}
	ranges := RangeOptions("/insights", nil, "30d")
	ins := sampleInsights()
	ins.Context = store.ContextHealthStats{} // no measured context

	html := renderComponent(t, InsightsPage(p, ins, "30d", ranges))

	if !strings.Contains(html, "No sessions with measured context in this window.") {
		t.Error("context band should note the missing measurements")
	}
	if !strings.Contains(html, `>Context health<`) {
		t.Error("the context band heading should still render")
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
