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

// sampleTrends is a small two-bucket trend grid touching every series the insights data
// serializer reads, so a page rendered with it draws the full instrument set and the
// embedded AK_DATA payload carries every field. The numbers are illustrative but internally
// consistent (shares sum sensibly, worst tools are a subset of the reliability list).
func sampleTrends() *store.Trends {
	rhythm := make([][]int, 7)
	for d := range rhythm {
		rhythm[d] = make([]int, 24)
	}
	rhythm[2][14] = 42 // a Wednesday-afternoon peak, so punchcardPeak has something to name

	return &store.Trends{
		Unit:         "week",
		BucketStarts: []time.Time{time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)},
		Labels:       []string{"Jun 1", "Jun 8"},
		FleetMix: store.FleetMix{Models: []store.ModelSeries{
			{Model: "claude-sonnet-5", Share: []float64{60, 55}, First: 0},
			{Model: "claude-opus-4-8", Share: []float64{40, 45}, First: 1},
		}},
		Gallery: store.Gallery{
			Rows: []store.GallerySession{
				{DurationS: 600, CostUSD: 1.2, Archetype: "quick", Grade: "A", Outcome: "completed"},
				{DurationS: 1800, CostUSD: 3.5, Archetype: "standard", Grade: "B", Outcome: "completed"},
				{DurationS: 3600, CostUSD: 8.4, Archetype: "deep", Grade: "C", Outcome: "abandoned"},
				{DurationS: 300, CostUSD: 0.4, Archetype: "quick", Grade: "A", Outcome: "completed"},
				{DurationS: 7200, CostUSD: 12.9, Archetype: "marathon", Grade: "B", Outcome: "completed"},
				{DurationS: 120, CostUSD: 0.1, Archetype: "automation", Grade: "", Outcome: "completed"},
				{DurationS: 2400, CostUSD: 5.1, Archetype: "deep", Grade: "A", Outcome: "completed"},
				{DurationS: 900, CostUSD: 1.9, Archetype: "standard", Grade: "B", Outcome: "abandoned"},
			},
			Total: 8,
			// Full-cohort summaries (store-computed in SQL): median of the eight durations is
			// 1350s and of the costs 2.7; the priciest and longest are the same marathon session
			// at $12.9 / 7200s; the six completed costs median to 2.35.
			MedianDurationS: 1350, MedianCostUSD: 2.7, MedianCompletedCostUSD: 2.35,
			PriciestDurationS: 7200, PriciestCostUSD: 12.9,
			LongestDurationS: 7200, LongestCostUSD: 12.9,
			CostIncomplete: true, // maps to the gallery's lower-bound cost markers
		},
		Velocity: store.VelocityTrends{
			ActiveHours: []float64{5, 6}, WallHours: []float64{8, 9},
			ResponseP50: []float64{12, 14}, ResponseP90: []float64{40, 44}, ResponseP99: []float64{90, 88},
			MsgsPerMin: []float64{3.1, 3.4}, ToolsPerMin: []float64{1.8, 2.0},
		},
		Tools: store.ToolTrends{
			Reliability: []store.ToolPoint{
				{Name: "Read", Calls: 100, Failures: 0, Sessions: 20, Category: "read"},
				{Name: "Edit", Calls: 60, Failures: 6, Sessions: 15, Category: "edit"},
				{Name: "Bash", Calls: 80, Failures: 8, Sessions: 18, Category: "bash"},
			},
			MixOrder: []string{"read", "edit", "bash", "other"},
			Mix: []map[string]float64{
				{"read": 45, "edit": 30, "bash": 20, "other": 5},
				{"read": 48, "edit": 28, "bash": 19, "other": 5},
			},
			FailFleet: []float64{2.1, 1.8},
			FailWorst: []store.ToolFailSeries{
				{Name: "Bash", Rate: []float64{5.0, 4.2}},
				{Name: "Edit", Rate: []float64{3.1, 2.8}},
			},
		},
		Churn: store.ChurnTrend{
			ReEdits: []int{12, 9}, Files: []int{5, 4},
			Tree: []store.ChurnNode{
				{Project: "akari", Folder: "internal/server/store", Path: "internal/server/store/analytics.go", Edits: 6, Sessions: 2},
				{Project: "akari", Folder: "internal/server/web", Path: "internal/server/web/insights.templ", Edits: 3, Sessions: 1},
			},
			// Three hot files in the window, two drawn in the tree, so one is clipped: the payload
			// carries the clipped count and the panel notes the tail.
			TotalReEdits: 21, TotalHotFiles: 3, Clipped: 1,
		},
		Signals: store.SignalTrends{
			GradeShare: []map[string]float64{
				{"A": 40, "B": 30, "C": 20, "D": 5, "F": 0, "": 5},
				{"A": 42, "B": 31, "C": 18, "D": 4, "F": 0, "": 5},
			},
			GPA:           []float64{3.1, 3.2},
			CompletedRate: []float64{70, 72}, AbandonedRate: []float64{10, 8}, OutcomeTotal: []int{15, 18},
			HygieneTerse: []float64{8, 7}, HygieneRepeated: []float64{3, 2},
			HygieneNoCode: []float64{5, 4}, HygieneUnstructured: []float64{6, 5},
			ContextResets: []int{2, 1},
			ContextHistogram: []store.ContextBucket{
				{Lo: 8000, Hi: 16000, Count: 3},
				{Lo: 16000, Hi: 32000, Count: 5},
			},
			ContextMarkers: []store.ContextMarker{{Tokens: 128000, Label: "p50 128.0k"}},
		},
		Economics: store.Economics{
			CostCompleted: []float64{100, 120}, CostAbandoned: []float64{20, 15}, CostOther: []float64{0, 0},
			CacheSavings: []float64{50, 60}, CacheHitRate: []float64{72, 74}, CacheMeasured: []bool{true, true},
			TotalSpend: 255, TotalAbandoned: 35, AbandonedSharePct: 13.7,
			TotalCacheSavings: 110, CacheHitRateLatest: 74,
			// Set so the serializer maps the lower-bound and partial markers through to the JSON.
			CostIncomplete: true, CacheSavingsIncomplete: true,
		},
		Subagents: store.SubagentStats{
			DelegateShare: []float64{20, 25}, CostShare: []float64{15, 18},
			FanoutOrder: []string{"one", "twoThree", "fourSeven", "eightPlus"},
			FanoutRows: []map[string]int{
				{"one": 3, "twoThree": 2, "fourSeven": 1, "eightPlus": 0},
				{"one": 4, "twoThree": 2, "fourSeven": 1, "eightPlus": 1},
			},
			SessionsThatDelegatePct: 22.5, SubagentSessionsInWindow: 14,
			CostThroughSubagentsPct: 16.4, DeepestTree: 3,
			CostShareIncomplete: true, // maps to the "partial" marker on the cost-share figure
		},
		Rhythm: store.RhythmGrid{Cells: rhythm},
	}
}

// sampleInsightsWithTrends is the fleet insights fixture: the distributions of
// sampleInsights plus a populated trend grid, so InsightsPage renders the full instrument
// scaffolding and its embedded data payload rather than the empty state.
func sampleInsightsWithTrends() store.Insights {
	ins := sampleInsights()
	ins.Trends = sampleTrends()
	return ins
}

// The Insights page renders the seven instrument sections' scaffolding (headings, tab
// strips, chart mount points), embeds the AK_DATA payload the chart engine reads, and wires
// the window selector to swap the whole section. The charts themselves draw client-side, so
// the server-rendered page is the static frame plus the data script.
func TestInsightsPageRendersInstruments(t *testing.T) {
	p := Page{Title: "Insights", LoggedIn: true, Active: "insights", Username: "ada"}
	ranges := RangeOptions("/insights", nil, "30d")
	html := renderComponent(t, InsightsPage(p, sampleInsightsWithTrends(), "30d", ranges))

	for _, want := range []string{
		`id="insights"`, // the swap target
		// the seven instrument headings
		`>Fleet mix<`, `>Session gallery<`, `>Velocity<`, `>Tools<`, `>Health<`, `>Economics<`, `>Subagents<`,
		// representative chart mount points the engine looks up by id
		`id="chart-fleetmix-full"`, `id="chart-gallery-full"`, `id="chart-punchcard"`,
		`id="chart-reliability"`, `id="treemap"`, `id="chart-grades"`, `id="chart-cache-full"`, `id="chart-fanout-full"`,
		// the cap-note hosts the engine fills when the scatter is a sample or the tree clipped hot files
		`id="gallery-sample"`, `id="churn-clip"`,
		// the four tab strips
		`id="velocity-tabs"`, `id="tools-tabs"`, `id="health-tabs"`, `id="economics-tabs"`,
		// a jump-to-tab mini multiple and the tooltip host
		`data-jump="velocity-tabs:vtab-activehours"`, `id="tooltip"`,
		// the embedded data payload and a couple of its keys, proving the serializer ran
		`id="insights-data"`, `"nBuckets":2`, `"bucketLabels":["Jun 1","Jun 8"]`, `"deepestTree":3`,
		// the live window selector: the active window is marked, the rest refetch and swap
		`aria-current="true"`, `hx-get="/insights?range=7d"`, `hx-select="#insights"`,
		// the session count in the header
		`15 session`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("insights page missing %q", want)
		}
	}

	// The old distribution-panel bars belong to the project quality band now, not the fleet
	// page, so the fleet page renders none of that server-side bar markup.
	for _, absent := range []string{`class="ins-grid"`, `class="mix-bar"`, `class="bar-fill"`} {
		if strings.Contains(html, absent) {
			t.Errorf("insights page should not render %q (that markup moved to the quality band)", absent)
		}
	}
}

// The range control drives the live window: every button refetches /insights for its window
// and swaps the whole #insights section (carrying the fresh data script), and only the
// active window is marked current.
func TestInsightsPageRangeControlIsLive(t *testing.T) {
	p := Page{Title: "Insights", LoggedIn: true, Active: "insights", Username: "ada"}
	ranges := RangeOptions("/insights", nil, "30d")
	html := renderComponent(t, InsightsPage(p, sampleInsightsWithTrends(), "30d", ranges))

	for _, want := range []string{
		`hx-get="/insights?range=7d"`,
		`hx-get="/insights?range=90d"`,
		`hx-target="#insights"`,
		`hx-swap="outerHTML"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("range control missing %q", want)
		}
	}
	// Exactly one button is marked current (the active 30d window).
	if got := strings.Count(html, `aria-current="true"`); got != 1 {
		t.Errorf("expected exactly one current range button, got %d", got)
	}
}

// With no trend grid the page shows the empty state and embeds no data script or instrument
// scaffolding, so the chart engine finds nothing to draw and stays a quiet no-op.
func TestInsightsPageEmptyState(t *testing.T) {
	p := Page{Title: "Insights", LoggedIn: true, Active: "insights", Username: "ada"}
	ranges := RangeOptions("/insights", nil, "7d")
	empty := store.Insights{} // Trends nil
	html := renderComponent(t, InsightsPage(p, empty, "7d", ranges))

	if !strings.Contains(html, "No sessions in this window yet.") {
		t.Error("empty insights page should show the empty state")
	}
	for _, absent := range []string{`id="insights-data"`, `id="chart-fleetmix-full"`, `id="velocity-tabs"`} {
		if strings.Contains(html, absent) {
			t.Errorf("empty insights page should not render %q", absent)
		}
	}
	// The swap target and live range control still render, so the empty page can refetch a
	// populated window without a full reload.
	if !strings.Contains(html, `id="insights"`) {
		t.Error("empty insights page should still carry the swap target")
	}
}
