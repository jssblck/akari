package web

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

// akData is the subset of the AK_DATA contract this test asserts on, typed so the JSON
// numbers land as the right Go kinds instead of untyped float64s. It intentionally does not
// cover every field; the render test proves the payload embeds, this proves the mapping.
type akData struct {
	NBuckets     int      `json:"nBuckets"`
	BucketUnit   string   `json:"bucketUnit"`
	BucketLabels []string `json:"bucketLabels"`

	FleetMix struct {
		Order              []string           `json:"order"`
		Colors             map[string]string  `json:"colors"`
		Labels             map[string]string  `json:"labels"`
		WindowShare        map[string]float64 `json:"windowShare"`
		ArrivalWeek        *int               `json:"arrivalWeek"`
		NewestArrivalLabel string             `json:"newestArrivalLabel"`
		NewestArrivalDate  string             `json:"newestArrivalDate"`
	} `json:"fleetMix"`

	SessionGallery struct {
		Points          []map[string]any  `json:"points"`
		ArchColor       map[string]string `json:"archColor"`
		MedianDurationS float64           `json:"medianDurationS"`
		CostIncomplete  bool              `json:"costIncomplete"`
		Total           int               `json:"total"`
		Shown           int               `json:"shown"`
		Priciest        struct {
			CostUsd float64 `json:"costUsd"`
		} `json:"priciest"`
	} `json:"sessionGallery"`

	Concurrency struct {
		AvgConcurrent  float64 `json:"avgConcurrent"`
		PeakConcurrent int     `json:"peakConcurrent"`
	} `json:"concurrency"`

	Throughput struct {
		MsgsPerMin    []float64 `json:"msgsPerMin"`
		ToolsPerMin   []float64 `json:"toolsPerMin"`
		MsgsPerMinAvg float64   `json:"msgsPerMinAvg"`
	} `json:"throughput"`

	AllTools []struct {
		Name string  `json:"name"`
		Err  float64 `json:"err"`
		Cat  string  `json:"cat"`
	} `json:"allTools"`

	ToolMix struct {
		Order  []string          `json:"order"`
		Colors map[string]string `json:"colors"`
		Labels map[string]string `json:"labels"`
	} `json:"toolMix"`

	ToolFailures struct {
		Fleet []float64 `json:"fleet"`
		Worst []struct {
			Name string    `json:"name"`
			Rate []float64 `json:"rate"`
		} `json:"worst"`
	} `json:"toolFailures"`

	Grades     []map[string]float64 `json:"grades"`
	Archetypes struct {
		Order  []string             `json:"order"`
		Colors map[string]string    `json:"colors"`
		Labels map[string]string    `json:"labels"`
		Rows   []map[string]float64 `json:"rows"`
	} `json:"archetypes"`
	Outcomes []map[string]float64 `json:"outcomes"`
	Hygiene  struct {
		NoPointer []float64 `json:"noPointer"`
	} `json:"hygiene"`

	ContextResets  []int `json:"contextResets"`
	ContextMarkers []struct {
		V     int64  `json:"v"`
		Label string `json:"label"`
	} `json:"contextMarkers"`
	ContextSummary struct {
		P50Label string `json:"p50Label"`
	} `json:"contextSummary"`

	Churn      []map[string]any    `json:"churn"`
	Projects   []string            `json:"projects"`
	FolderPlan map[string][]string `json:"folderPlan"`
	ChurnTrend struct {
		TotalHotFiles int `json:"totalHotFiles"`
		TotalReedits  int `json:"totalReedits"`
		Clipped       int `json:"clipped"`
		ProjectCount  int `json:"projectCount"`
	} `json:"churnTrend"`

	CostQuality struct {
		TotalSpend                float64 `json:"totalSpend"`
		TotalSpendIncomplete      bool    `json:"totalSpendIncomplete"`
		MedianPerCompletedSession float64 `json:"medianPerCompletedSession"`
	} `json:"costQuality"`

	Cache struct {
		TotalSavings      float64 `json:"totalSavings"`
		SavingsIncomplete bool    `json:"savingsIncomplete"`
		HitRateNow        float64 `json:"hitRateNow"`
	} `json:"cache"`

	Subagents struct {
		DeepestTree                int  `json:"deepestTree"`
		SessionsThatDelegatePct    int  `json:"sessionsThatDelegatePct"`
		CostRunThroughSubagentsPct int  `json:"costRunThroughSubagentsPct"`
		CostShareIncomplete        bool `json:"costShareIncomplete"`
	} `json:"subagents"`

	Punchcard [][]struct {
		Volume int `json:"volume"`
	} `json:"punchcard"`
	PunchcardPeakLabel string `json:"punchcardPeakLabel"`
}

// InsightsData maps the store trend types into the AK_DATA shape the chart engine reads.
// This checks the load-bearing translations: the shared grid, the ranked-series colouring,
// the derived figures (medians, arrival marker), the two integration-time adaptations
// (throughput cadence and dynamic worst-tool failures), and the fields that come from
// outside the grid (concurrency, context percentile).
func TestInsightsDataMapping(t *testing.T) {
	ins := sampleInsightsWithTrends()
	raw, err := InsightsData(ins)
	if err != nil {
		t.Fatalf("InsightsData: %v", err)
	}
	var d akData
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}

	// The shared bucket grid.
	if d.NBuckets != 2 || d.BucketUnit != "week" {
		t.Errorf("grid: got nBuckets=%d unit=%q, want 2/week", d.NBuckets, d.BucketUnit)
	}
	if len(d.BucketLabels) != 2 || d.BucketLabels[0] != "Jun 1" || d.BucketLabels[1] != "Jun 8" {
		t.Errorf("bucketLabels = %v, want [Jun 1 Jun 8]", d.BucketLabels)
	}

	// Fleet mix: models ranked with palette colours and a mid-window arrival marker. Opus
	// first appears in bucket 1 (First:1), so it is the newest arrival.
	if len(d.FleetMix.Order) != 2 {
		t.Fatalf("fleetMix.order = %v, want 2 models", d.FleetMix.Order)
	}
	if d.FleetMix.Colors["claude-sonnet-5"] == "" {
		t.Error("fleetMix.colors missing the sonnet swatch")
	}
	if d.FleetMix.Labels["claude-opus-4-8"] != "opus-4-8" {
		t.Errorf("fleetMix.labels[opus] = %q, want opus-4-8 (prettified)", d.FleetMix.Labels["claude-opus-4-8"])
	}
	// The whole-window shares ride along per model: the busiest-model figure reads these,
	// not the trailing (partial, often empty) bucket's row.
	if d.FleetMix.WindowShare["claude-sonnet-5"] != 57.5 || d.FleetMix.WindowShare["claude-opus-4-8"] != 42.5 {
		t.Errorf("fleetMix.windowShare = %v, want sonnet 57.5 / opus 42.5", d.FleetMix.WindowShare)
	}
	if d.FleetMix.ArrivalWeek == nil || *d.FleetMix.ArrivalWeek != 1 {
		t.Errorf("fleetMix.arrivalWeek = %v, want 1", d.FleetMix.ArrivalWeek)
	}
	if d.FleetMix.NewestArrivalDate != "Jun 8" {
		t.Errorf("fleetMix.newestArrivalDate = %q, want Jun 8", d.FleetMix.NewestArrivalDate)
	}

	// Gallery: one point per session, the automation archetype coloured, the priciest and
	// median duration derived.
	if len(d.SessionGallery.Points) != 8 {
		t.Errorf("gallery has %d points, want 8", len(d.SessionGallery.Points))
	}
	if d.SessionGallery.ArchColor["automation"] == "" {
		t.Error("gallery archColor missing the automation swatch")
	}
	if d.SessionGallery.Priciest.CostUsd != 12.9 {
		t.Errorf("gallery priciest = %v, want 12.9", d.SessionGallery.Priciest.CostUsd)
	}
	// The scatter's shown/total drive the sample note: the fixture's cohort fits under the cap, so
	// they are equal here, but the serializer must carry both so a >cap window can note the sample.
	if d.SessionGallery.Shown != 8 || d.SessionGallery.Total != 8 {
		t.Errorf("gallery shown/total = %d/%d, want 8/8", d.SessionGallery.Shown, d.SessionGallery.Total)
	}

	// Concurrency comes from outside the grid (ConcurrencyStats).
	if d.Concurrency.PeakConcurrent != 4 || d.Concurrency.AvgConcurrent != 1.7 {
		t.Errorf("concurrency = %+v, want peak 4 / avg 1.7", d.Concurrency)
	}

	// Throughput is the integration-time cadence adaptation: per-bucket msgs/min and
	// tools/min, not the underivable per-model tokens/sec. The headline avg is the canonical
	// whole-window rate (VelocityStats.MsgsPerActiveMin), not the mean of the per-bucket series.
	if len(d.Throughput.MsgsPerMin) != 2 || d.Throughput.MsgsPerMin[0] != 3.1 {
		t.Errorf("throughput.msgsPerMin = %v, want [3.1 3.4]", d.Throughput.MsgsPerMin)
	}
	if d.Throughput.MsgsPerMinAvg != 4.2 {
		t.Errorf("throughput.msgsPerMinAvg = %v, want 4.2 (canonical MsgsPerActiveMin, not the per-bucket mean 3.25)", d.Throughput.MsgsPerMinAvg)
	}

	// Tools: reliability error rate is a percent, the mix legend uses the shared category
	// map (bash reads as Shell / viz-1), and failures carry the dynamic worst list.
	var edit struct {
		found bool
		err   float64
	}
	for _, tool := range d.AllTools {
		if tool.Name == "Edit" {
			edit.found, edit.err = true, tool.Err
		}
	}
	if !edit.found || math.Abs(edit.err-10) > 0.001 {
		t.Errorf("allTools Edit err = %v (found=%v), want 10%% (6/60)", edit.err, edit.found)
	}
	if d.ToolMix.Colors["bash"] != "var(--viz-1)" || d.ToolMix.Labels["bash"] != "Shell" {
		t.Errorf("toolMix bash style = %q/%q, want var(--viz-1)/Shell", d.ToolMix.Colors["bash"], d.ToolMix.Labels["bash"])
	}
	if len(d.ToolFailures.Worst) == 0 || d.ToolFailures.Worst[0].Name != "Bash" {
		t.Errorf("toolFailures.worst = %+v, want Bash first", d.ToolFailures.Worst)
	}
	if len(d.ToolFailures.Fleet) != 2 || d.ToolFailures.Fleet[0] != 2.1 {
		t.Errorf("toolFailures.fleet = %v, want [2.1 1.8]", d.ToolFailures.Fleet)
	}

	// Health: grades carry the unscored share as U, outcomes carry the raw denominator, and
	// hygiene's no-code figure maps to noPointer.
	if len(d.Grades) != 2 || d.Grades[0]["U"] != 5 || d.Grades[0]["gpa"] != 3.1 {
		t.Errorf("grades[0] = %v, want U=5 gpa=3.1", d.Grades[0])
	}
	if len(d.Outcomes) != 2 || d.Outcomes[0]["total"] != 15 {
		t.Errorf("outcomes[0].total = %v, want 15", d.Outcomes[0]["total"])
	}
	// The raw completed/abandoned counts back the magnitude bars' three-band partition; other is
	// the residue (15 - 10 - 2 = 3). Without them the bar would derive a warn segment as
	// total-completed and colour the errored/unknown sessions as abandoned.
	if d.Outcomes[0]["completed"] != 10 || d.Outcomes[0]["abandoned"] != 2 {
		t.Errorf("outcomes[0] completed/abandoned = %v/%v, want 10/2", d.Outcomes[0]["completed"], d.Outcomes[0]["abandoned"])
	}
	if len(d.Hygiene.NoPointer) != 2 || d.Hygiene.NoPointer[0] != 5 {
		t.Errorf("hygiene.noPointer = %v, want [5 4]", d.Hygiene.NoPointer)
	}

	// Archetypes: the per-bucket mix carries the fixed lightest-to-heaviest order, the shared
	// swatch map (quick reads as viz-2), and each row's shares (bucket 0 quick = 40%). This is
	// the series the project page's Quality instrument draws; /insights carries it but mounts
	// no archetype chart.
	if len(d.Archetypes.Order) != 5 || d.Archetypes.Order[0] != "quick" || d.Archetypes.Order[4] != "automation" {
		t.Errorf("archetypes.order = %v, want quick..automation", d.Archetypes.Order)
	}
	if d.Archetypes.Colors["quick"] != "var(--viz-2)" || d.Archetypes.Labels["quick"] != "Quick" {
		t.Errorf("archetypes quick style = %q/%q, want var(--viz-2)/Quick", d.Archetypes.Colors["quick"], d.Archetypes.Labels["quick"])
	}
	if len(d.Archetypes.Rows) != 2 || d.Archetypes.Rows[0]["quick"] != 40 {
		t.Errorf("archetypes.rows[0].quick = %v, want 40", d.Archetypes.Rows[0])
	}

	// Context: resets per bucket, the p50/p90/max markers, and the summary label from the
	// window's context percentile (128000 tokens -> 128.0k).
	if len(d.ContextResets) != 2 || d.ContextResets[0] != 2 {
		t.Errorf("contextResets = %v, want [2 1]", d.ContextResets)
	}
	if len(d.ContextMarkers) == 0 || d.ContextMarkers[0].Label != "p50 128.0k" {
		t.Errorf("contextMarkers = %+v, want a p50 128.0k marker", d.ContextMarkers)
	}
	if d.ContextSummary.P50Label != "128.0k" {
		t.Errorf("contextSummary.p50Label = %q, want 128.0k", d.ContextSummary.P50Label)
	}

	// Churn: the tree, the project/folder drill lists, and the window totals.
	if len(d.Churn) != 2 {
		t.Errorf("churn has %d rows, want 2", len(d.Churn))
	}
	if len(d.Projects) != 1 || d.Projects[0] != "akari" {
		t.Errorf("projects = %v, want [akari]", d.Projects)
	}
	if len(d.FolderPlan["akari"]) != 2 {
		t.Errorf("folderPlan[akari] = %v, want 2 folders", d.FolderPlan["akari"])
	}
	if d.ChurnTrend.TotalHotFiles != 3 || d.ChurnTrend.TotalReedits != 21 {
		t.Errorf("churnTrend totals = %+v, want 3 hot files / 21 re-edits", d.ChurnTrend)
	}
	// The clipped count feeds the treemap tail note: three hot files, two in the tree, so one is
	// clipped and the serializer must carry it or the headline would exceed the visible breakdown.
	if d.ChurnTrend.Clipped != 1 {
		t.Errorf("churnTrend clipped = %d, want 1", d.ChurnTrend.Clipped)
	}
	// The uncapped project span drives the treemap's single-project shortcut. All the fixture's hot
	// files sit in one project, so it reads 1; the JS reads this rather than the capped tree's
	// project list, which a multi-project window could shrink to one.
	if d.ChurnTrend.ProjectCount != 1 {
		t.Errorf("churnTrend projectCount = %d, want 1 (uncapped single-project cohort)", d.ChurnTrend.ProjectCount)
	}

	// Economics: the median completed-session cost is read off the gallery cohort. Completed
	// costs are {1.2, 3.5, 0.4, 12.9, 0.1, 5.1}; sorted the two middle values are 1.2 and
	// 3.5, so the median is 2.35.
	if d.CostQuality.TotalSpend != 255 {
		t.Errorf("costQuality.totalSpend = %v, want 255", d.CostQuality.TotalSpend)
	}
	if math.Abs(d.CostQuality.MedianPerCompletedSession-2.35) > 0.001 {
		t.Errorf("costQuality.medianPerCompletedSession = %v, want 2.35", d.CostQuality.MedianPerCompletedSession)
	}
	if d.Cache.TotalSavings != 110 || d.Cache.HitRateNow != 74 {
		t.Errorf("cache = %+v, want 110 saved / 74%% now", d.Cache)
	}
	// The incompleteness flags map through so the client can mark cost figures as lower-bound
	// ("+") and savings/share figures as partial, matching the canonical cost and cache surfaces.
	if !d.CostQuality.TotalSpendIncomplete {
		t.Error("costQuality.totalSpendIncomplete = false, want true (Economics.CostIncomplete set)")
	}
	if !d.SessionGallery.CostIncomplete {
		t.Error("sessionGallery.costIncomplete = false, want true (Gallery.CostIncomplete set)")
	}
	if !d.Cache.SavingsIncomplete {
		t.Error("cache.savingsIncomplete = false, want true (Economics.CacheSavingsIncomplete set)")
	}
	if !d.Subagents.CostShareIncomplete {
		t.Error("subagents.costShareIncomplete = false, want true (SubagentStats.CostShareIncomplete set)")
	}

	// Subagents: the tree depth and the two whole-number percentages the JS concatenates a
	// '%' onto (22.5 rounds away from zero to 23, 16.4 to 16).
	if d.Subagents.DeepestTree != 3 {
		t.Errorf("subagents.deepestTree = %d, want 3", d.Subagents.DeepestTree)
	}
	if d.Subagents.SessionsThatDelegatePct != 23 || d.Subagents.CostRunThroughSubagentsPct != 16 {
		t.Errorf("subagents pcts = %d/%d, want 23/16", d.Subagents.SessionsThatDelegatePct, d.Subagents.CostRunThroughSubagentsPct)
	}

	// Rhythm: a 7x24 grid with the seeded Wednesday-afternoon peak surfaced as the caption.
	if len(d.Punchcard) != 7 || len(d.Punchcard[0]) != 24 {
		t.Fatalf("punchcard is %dx?, want 7x24", len(d.Punchcard))
	}
	if d.Punchcard[2][14].Volume != 42 {
		t.Errorf("punchcard[Wed][14] = %d, want 42", d.Punchcard[2][14].Volume)
	}
	if d.PunchcardPeakLabel != "peak Wed 14:00" {
		t.Errorf("punchcardPeakLabel = %q, want peak Wed 14:00", d.PunchcardPeakLabel)
	}
}

// A trend grid with no data (nil Trends) has no payload to serialize, so the serializer
// reports an error rather than emitting a misleading empty object; the page uses this as the
// empty-state signal.
func TestInsightsDataNoTrends(t *testing.T) {
	if _, err := InsightsData(sampleInsights()); err == nil {
		t.Error("InsightsData should error when there is no trend grid")
	}
}

// TestFleetMixArrivalSurvivesTheFold pins the fix for the cross-window "newest arrival"
// disagreement: on a long window a just-arrived model's whole-window token share is tiny,
// so the top-N fold pushes it into "other" and it appears in no kept band. The callout must
// still name it, because it reads the store's fleet-history scan (FleetMix.NewestModel), not
// the bands.
func TestFleetMixArrivalSurvivesTheFold(t *testing.T) {
	labels := []string{"Jun 1", "Jun 8", "Jun 15"}
	fm := store.FleetMix{
		Models: []store.ModelSeries{
			{Model: "claude-sonnet-5", Share: []float64{60, 55, 50}, First: 0},
			{Model: "other", Share: []float64{40, 45, 50}, First: 0},
		},
		NewestModel: "gpt-5.6-luna",
		NewestFirst: 2,
	}
	out := fleetMixData(fm, labels, 3)
	if got := out["newestArrivalLabel"]; got != "gpt-5.6-luna" {
		t.Errorf("newestArrivalLabel = %v, want gpt-5.6-luna (the folded arrival)", got)
	}
	if got := out["arrivalWeek"]; got != 2 {
		t.Errorf("arrivalWeek = %v, want 2", got)
	}
	if got := out["newestArrivalDate"]; got != "Jun 15" {
		t.Errorf("newestArrivalDate = %v, want Jun 15", got)
	}

	// No mid-window arrival (every model an incumbent): the callout stays absent.
	fm.NewestModel, fm.NewestFirst = "", -1
	out = fleetMixData(fm, labels, 3)
	if _, ok := out["newestArrivalLabel"]; ok {
		t.Error("newestArrivalLabel present with no mid-window arrival")
	}
	if _, ok := out["arrivalWeek"]; ok {
		t.Error("arrivalWeek present with no mid-window arrival")
	}
}
