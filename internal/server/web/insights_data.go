package web

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/a-h/templ"
	"github.com/jssblck/akari/internal/server/store"
)

// InsightsData serializes the trend grid into the single JSON blob the insights chart
// engine reads from #insights-data as window.AK_DATA. It is the one bridge between the Go
// store types and static/js/insights.js: every field name here is a contract the JS depends
// on, so a rename on either side must move together. It returns the marshaled JSON; the
// template embeds it raw in a <script type="application/json"> (json.Marshal escapes <, >,
// and & to \uXXXX, so the payload can never break out of the script element).
//
// It reads the whole Insights value, not just Trends, because a few headline figures the
// charts annotate live outside the grid: the concurrency peak/average and the window's
// context percentile. When there is no trend grid the caller renders an empty state instead
// of calling this.
func InsightsData(ins store.Insights) (string, error) {
	t := ins.Trends
	if t == nil {
		return "", fmt.Errorf("insights data: no trend grid")
	}
	n := len(t.BucketStarts)

	payload := map[string]any{
		"nBuckets":     n,
		"bucketUnit":   t.Unit,
		"bucketLabels": t.Labels,

		"fleetMix":       fleetMixData(t.FleetMix, t.Labels, n),
		"sessionGallery": galleryData(t.Gallery),
		"concurrency": map[string]any{
			"avgConcurrent":  ins.Concurrency.AvgConcurrent,
			"peakConcurrent": ins.Concurrency.FleetPeak,
		},
		"activeHours":  activeHoursData(t.Velocity),
		"responseTime": map[string]any{"p50": t.Velocity.ResponseP50, "p90": t.Velocity.ResponseP90, "p99": t.Velocity.ResponseP99},
		"throughput":   map[string]any{"msgsPerMin": t.Velocity.MsgsPerMin, "toolsPerMin": t.Velocity.ToolsPerMin},

		"allTools":     toolsData(t.Tools.Reliability),
		"toolMix":      toolMixData(t.Tools, n),
		"toolFailures": failuresData(t.Tools),

		"grades":           gradesData(t.Signals),
		"outcomes":         outcomesData(t.Signals),
		"hygiene":          hygieneData(t.Signals),
		"contextHistogram": histogramData(t.Signals.ContextHistogram),
		"contextResets":    t.Signals.ContextResets,

		"churn":       churnRows(t.Churn.Tree),
		"projects":    churnProjects(t.Churn.Tree),
		"projectViz":  churnProjectColors(t.Churn.Tree),
		"folderPlan":  churnFolderPlan(t.Churn.Tree),
		"churnTrend":  churnTrendData(t.Churn),
		"costQuality": costQualityData(t.Economics, t.Gallery),
		"cache":       cacheData(t.Economics),

		"subagents": subagentsData(t.Subagents),
		"punchcard": punchcardData(t.Rhythm),
	}

	// Optional annotations: emitted only when there is something to point at, so the JS
	// treats their absence as "nothing to draw" rather than an empty marker.
	if idx, label, ok := activeHoursPeak(t.Velocity, t.Labels); ok {
		ah := payload["activeHours"].(map[string]any)
		ah["maxIdx"] = idx
		ah["maxLabel"] = label
	}
	if label, ok := punchcardPeak(t.Rhythm); ok {
		payload["punchcardPeakLabel"] = label
	}
	if markers := contextMarkers(t.Signals.ContextMarkers); len(markers) > 0 {
		payload["contextMarkers"] = markers
	}
	if ins.Context.HasData() {
		payload["contextSummary"] = map[string]any{"p50Label": fmtTokensShort(ins.Context.PeakTokensP50)}
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("insights data: %w", err)
	}
	return string(out), nil
}

// insightsDataElement is the template-facing wrapper: it returns the whole #insights-data
// script element with the AK_DATA JSON inside, as a raw component. It is built in Go rather
// than in the template because templ treats a <script> element's contents as opaque text and
// would emit an @-expression literally rather than executing it. The JSON is safe to embed
// raw: json.Marshal escapes <, >, and & to \uXXXX, so no </script> can appear in it to close
// the element early. A marshal failure yields an empty element, which the JS reads as "no
// data" and no-ops on; the page only calls this when the grid has data, so that is the error
// path, not the normal one.
func insightsDataElement(ins store.Insights) templ.Component {
	data, err := InsightsData(ins)
	if err != nil {
		data = ""
	}
	return templ.Raw(`<script id="insights-data" type="application/json">` + data + `</script>`)
}

// vizVars is the ordered accent ramp the charts assign to ranked series (models, projects),
// named as the CSS custom properties base.css defines rather than raw hex, so a series
// recolours with the theme. It parallels the hex vizPalette used elsewhere in this package
// (var(--viz-1) resolves to vizPalette[0]); the SVG engine reads these var() forms.
var vizVars = []string{
	"var(--viz-1)", "var(--viz-2)", "var(--viz-3)", "var(--viz-4)",
	"var(--viz-5)", "var(--viz-6)", "var(--viz-7)", "var(--viz-8)",
}

// categoryStyle maps each tool category to its swatch and label, mirroring the CAT_COLOR /
// CAT_LABEL maps in insights.js so the reliability scatter (which reads the JS map) and the
// mix legend (which reads this one, embedded in toolMix) agree. The vocabulary is the
// parser's fixed set (internal/parser.toolCategory): bash / edit / read / search / write,
// with the unclassified tail as other.
var categoryStyle = map[string][2]string{
	"bash":   {"var(--viz-1)", "Shell"},
	"edit":   {"var(--viz-4)", "Edit"},
	"read":   {"var(--viz-2)", "Read"},
	"search": {"var(--viz-6)", "Search"},
	"write":  {"var(--viz-3)", "Write"},
	"other":  {"var(--viz-8)", "Other"},
}

func categoryColor(cat string) string {
	if s, ok := categoryStyle[cat]; ok {
		return s[0]
	}
	return "var(--viz-8)"
}

func categoryLabel(cat string) string {
	if s, ok := categoryStyle[cat]; ok {
		return s[1]
	}
	return titleCase(cat)
}

// archetypeColor maps a session archetype to its gallery swatch, matching the ARCH_LABEL
// keys in insights.js (quick / standard / deep / marathon / automation).
var archetypeColor = map[string]string{
	"quick":      "var(--viz-2)",
	"standard":   "var(--viz-4)",
	"deep":       "var(--viz-6)",
	"marathon":   "var(--viz-7)",
	"automation": "var(--viz-8)",
}

func fleetMixData(fm store.FleetMix, labels []string, n int) map[string]any {
	order := make([]string, 0, len(fm.Models))
	colors := map[string]string{}
	modelLabels := map[string]string{}
	rows := make([]map[string]float64, n)
	for i := range rows {
		rows[i] = map[string]float64{}
	}
	// arrivalWeek tracks the latest bucket any tracked model first appears in, so the chart
	// can mark a mid-window model migration. The "other" fold is skipped: its arrival is not
	// a single model's story.
	arrival := -1
	arrivalModel := ""
	pi := 0
	for _, m := range fm.Models {
		order = append(order, m.Model)
		if m.Model == "other" {
			colors[m.Model] = "var(--muted)"
		} else {
			colors[m.Model] = vizVars[pi%len(vizVars)]
			pi++
		}
		modelLabels[m.Model] = prettyModel(m.Model)
		for i := 0; i < n && i < len(m.Share); i++ {
			rows[i][m.Model] = m.Share[i]
		}
		if m.Model != "other" && m.First > arrival {
			arrival = m.First
			arrivalModel = m.Model
		}
	}
	out := map[string]any{"order": order, "colors": colors, "labels": modelLabels, "rows": rows}
	if arrival > 0 {
		out["arrivalWeek"] = arrival
		out["newestArrivalLabel"] = prettyModel(arrivalModel)
		if arrival < len(labels) {
			out["newestArrivalDate"] = labels[arrival]
		}
	}
	return out
}

// prettyModel shortens a model identifier for a legend chip: the raw usage_events.model can
// be a long slug, and the migration story reads better on the family name.
func prettyModel(m string) string {
	if m == "" || m == "unknown" {
		return "unknown"
	}
	s := strings.TrimPrefix(m, "claude-")
	s = strings.TrimPrefix(s, "anthropic/")
	return s
}

func galleryData(g store.Gallery) map[string]any {
	points := make([]map[string]any, 0, len(g.Rows))
	durations := make([]float64, 0, len(g.Rows))
	costs := make([]float64, 0, len(g.Rows))
	var priciest store.GallerySession
	for _, r := range g.Rows {
		points = append(points, map[string]any{
			"durationS": r.DurationS, "costUsd": r.CostUSD, "arch": r.Archetype,
			"grade": r.Grade, "outcome": r.Outcome,
		})
		durations = append(durations, r.DurationS)
		costs = append(costs, r.CostUSD)
		if r.CostUSD > priciest.CostUSD {
			priciest = r
		}
	}
	out := map[string]any{
		"points":          points,
		"archColor":       archetypeColor,
		"medianDurationS": median(durations),
		"medianCostUsd":   median(costs),
		"priciest":        map[string]any{"durationS": priciest.DurationS, "costUsd": priciest.CostUSD},
	}
	// A couple of callouts, only when the scatter has enough points that a labelled outlier
	// is not the whole story: the priciest session, and the longest-running one.
	if len(g.Rows) >= 8 {
		var longest store.GallerySession
		for _, r := range g.Rows {
			if r.DurationS > longest.DurationS {
				longest = r
			}
		}
		anns := []map[string]any{
			{"durationS": priciest.DurationS, "costUsd": priciest.CostUSD, "label": fmtCostShort(priciest.CostUSD), "corner": "top-right"},
		}
		if longest.DurationS != priciest.DurationS {
			anns = append(anns, map[string]any{"durationS": longest.DurationS, "costUsd": longest.CostUSD, "label": fmtDurationShort(longest.DurationS), "corner": "bottom-left"})
		}
		out["annotations"] = anns
	}
	return out
}

func activeHoursData(v store.VelocityTrends) map[string]any {
	return map[string]any{"active": v.ActiveHours, "wallSpan": v.WallHours}
}

// activeHoursPeak finds the busiest bucket by hands-on hours, for the peak marker. It
// reports false when every bucket is idle, so the marker is skipped rather than pinned to an
// arbitrary zero.
func activeHoursPeak(v store.VelocityTrends, labels []string) (int, string, bool) {
	idx, best := -1, 0.0
	for i, h := range v.ActiveHours {
		if h > best {
			best, idx = h, i
		}
	}
	if idx < 0 {
		return 0, "", false
	}
	label := fmt.Sprintf("peak %s · %.1fh", labels[idx], v.ActiveHours[idx])
	return idx, label, true
}

func toolsData(rel []store.ToolPoint) []map[string]any {
	out := make([]map[string]any, 0, len(rel))
	for _, t := range rel {
		out = append(out, map[string]any{
			"name": t.Name, "calls": t.Calls, "err": t.ErrorRate(),
			"sessions": t.Sessions, "cat": t.Category,
		})
	}
	return out
}

func toolMixData(tt store.ToolTrends, n int) map[string]any {
	colors := map[string]string{}
	labels := map[string]string{}
	for _, cat := range tt.MixOrder {
		colors[cat] = categoryColor(cat)
		labels[cat] = categoryLabel(cat)
	}
	rows := make([]map[string]float64, n)
	for i := range rows {
		if i < len(tt.Mix) && tt.Mix[i] != nil {
			rows[i] = tt.Mix[i]
		} else {
			rows[i] = map[string]float64{}
		}
	}
	out := map[string]any{"order": tt.MixOrder, "colors": colors, "labels": labels, "rows": rows}
	if len(tt.MixOrder) > 0 {
		out["miniLabel"] = categoryLabel(tt.MixOrder[0]) + " dominant"
	}
	return out
}

func failuresData(tt store.ToolTrends) map[string]any {
	worst := make([]map[string]any, 0, len(tt.FailWorst))
	for _, s := range tt.FailWorst {
		worst = append(worst, map[string]any{"name": s.Name, "rate": s.Rate})
	}
	return map[string]any{"fleet": tt.FailFleet, "worst": worst}
}

func gradesData(s store.SignalTrends) []map[string]any {
	out := make([]map[string]any, len(s.GradeShare))
	for i, share := range s.GradeShare {
		out[i] = map[string]any{
			"A": share["A"], "B": share["B"], "C": share["C"],
			"D": share["D"], "F": share["F"], "U": share[""],
			"gpa": s.GPA[i],
		}
	}
	return out
}

func outcomesData(s store.SignalTrends) []map[string]any {
	out := make([]map[string]any, len(s.CompletedRate))
	for i := range s.CompletedRate {
		out[i] = map[string]any{
			"completedRate": s.CompletedRate[i],
			"abandonedRate": s.AbandonedRate[i],
			"total":         s.OutcomeTotal[i],
		}
	}
	return out
}

func hygieneData(s store.SignalTrends) map[string]any {
	return map[string]any{
		"terse":        s.HygieneTerse,
		"repeated":     s.HygieneRepeated,
		"noPointer":    s.HygieneNoCode,
		"unstructured": s.HygieneUnstructured,
	}
}

func histogramData(bins []store.ContextBucket) []map[string]any {
	out := make([]map[string]any, len(bins))
	for i, b := range bins {
		out[i] = map[string]any{"lo": b.Lo, "hi": b.Hi, "count": b.Count}
	}
	return out
}

func contextMarkers(markers []store.ContextMarker) []map[string]any {
	out := make([]map[string]any, 0, len(markers))
	for _, m := range markers {
		out = append(out, map[string]any{"v": m.Tokens, "label": m.Label})
	}
	return out
}

func churnRows(tree []store.ChurnNode) []map[string]any {
	out := make([]map[string]any, 0, len(tree))
	for _, node := range tree {
		out = append(out, map[string]any{
			"project": node.Project, "folder": node.Folder, "path": node.Path,
			"edits": node.Edits, "sessions": node.Sessions,
		})
	}
	return out
}

// churnProjects lists the churned projects in first-seen (busiest-first) order, the drill
// root the treemap opens on.
func churnProjects(tree []store.ChurnNode) []string {
	seen := map[string]bool{}
	var out []string
	for _, node := range tree {
		if !seen[node.Project] {
			seen[node.Project] = true
			out = append(out, node.Project)
		}
	}
	return out
}

func churnProjectColors(tree []store.ChurnNode) map[string]string {
	out := map[string]string{}
	i := 0
	for _, p := range churnProjects(tree) {
		out[p] = vizVars[i%len(vizVars)]
		i++
	}
	return out
}

// churnFolderPlan lists each project's folders in first-seen order, the middle drill level
// of the treemap.
func churnFolderPlan(tree []store.ChurnNode) map[string][]string {
	out := map[string][]string{}
	seen := map[string]bool{}
	for _, node := range tree {
		key := node.Project + "\x00" + node.Folder
		if seen[key] {
			continue
		}
		seen[key] = true
		out[node.Project] = append(out[node.Project], node.Folder)
	}
	return out
}

func churnTrendData(c store.ChurnTrend) map[string]any {
	return map[string]any{
		"reedits":       c.ReEdits,
		"hotFiles":      c.Files,
		"totalReedits":  c.TotalReEdits,
		"totalHotFiles": c.TotalHotFiles,
	}
}

func costQualityData(e store.Economics, g store.Gallery) map[string]any {
	rows := make([]map[string]any, len(e.CostCompleted))
	for i := range e.CostCompleted {
		rows[i] = map[string]any{"completed": e.CostCompleted[i], "abandoned": e.CostAbandoned[i]}
	}
	// The median completed session's cost, read off the gallery cohort (the same fully
	// spanned sessions), so the figure is a real per-session dollar amount rather than a
	// spend total divided by a guessed session count.
	var completedCosts []float64
	for _, r := range g.Rows {
		if r.Outcome == "completed" {
			completedCosts = append(completedCosts, r.CostUSD)
		}
	}
	return map[string]any{
		"rows":                      rows,
		"totalSpend":                e.TotalSpend,
		"totalAbandoned":            e.TotalAbandoned,
		"abandonedSharePct":         e.AbandonedSharePct,
		"medianPerCompletedSession": median(completedCosts),
	}
}

func cacheData(e store.Economics) map[string]any {
	return map[string]any{
		"savings":      e.CacheSavings,
		"hitRate":      e.CacheHitRate,
		"totalSavings": e.TotalCacheSavings,
		"hitRateNow":   e.CacheHitRateLatest,
	}
}

// fanoutColors is the ordinal ramp for the delegation fan-out stack: one lilac hue
// (matching the punchcard accent) deepening as the fan-out widens, so "8+ subagents" reads
// as the strongest band and "1" as the faintest.
var fanoutColors = map[string]string{
	"one":       "rgba(198,168,242,0.30)",
	"twoThree":  "rgba(198,168,242,0.52)",
	"fourSeven": "rgba(198,168,242,0.74)",
	"eightPlus": "rgba(198,168,242,0.96)",
}

var fanoutLabels = map[string]string{
	"one":       "1",
	"twoThree":  "2-3",
	"fourSeven": "4-7",
	"eightPlus": "8+",
}

func subagentsData(s store.SubagentStats) map[string]any {
	fanoutRows := make([]map[string]int, len(s.FanoutRows))
	for i, r := range s.FanoutRows {
		if r != nil {
			fanoutRows[i] = r
		} else {
			fanoutRows[i] = map[string]int{}
		}
	}
	return map[string]any{
		"delegateShare":              s.DelegateShare,
		"costShare":                  s.CostShare,
		"fanoutOrder":                s.FanoutOrder,
		"fanoutColors":               fanoutColors,
		"fanoutLabels":               fanoutLabels,
		"fanoutRows":                 fanoutRows,
		"sessionsThatDelegatePct":    int(math.Round(s.SessionsThatDelegatePct)),
		"subagentSessionsInWindow":   s.SubagentSessionsInWindow,
		"costRunThroughSubagentsPct": int(math.Round(s.CostThroughSubagentsPct)),
		"deepestTree":                s.DeepestTree,
	}
}

// punchcardDOW labels the punchcard rows Monday-first, matching the DOW array in
// insights.js and the isodow indexing the rhythm grid uses (0 = Monday).
var punchcardDOW = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

func punchcardData(r store.RhythmGrid) [][]map[string]int {
	out := make([][]map[string]int, len(r.Cells))
	for d, row := range r.Cells {
		cells := make([]map[string]int, len(row))
		for h, v := range row {
			cells[h] = map[string]int{"volume": v}
		}
		out[d] = cells
	}
	return out
}

// punchcardPeak names the busiest hour of the week for the rhythm caption, or reports false
// when the grid is empty so the caption is left blank rather than pointing at midnight
// Monday by default.
func punchcardPeak(r store.RhythmGrid) (string, bool) {
	bestD, bestH, best := -1, -1, 0
	for d, row := range r.Cells {
		for h, v := range row {
			if v > best {
				best, bestD, bestH = v, d, h
			}
		}
	}
	if bestD < 0 {
		return "", false
	}
	return fmt.Sprintf("peak %s %02d:00", punchcardDOW[bestD], bestH), true
}

// median returns the middle value of a copy of xs (mean of the two middle values for an even
// count), or 0 for an empty slice.
func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}

func fmtTokensShort(nTok int64) string {
	switch {
	case nTok >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(nTok)/1_000_000)
	case nTok >= 1000:
		return fmt.Sprintf("%.1fk", float64(nTok)/1000)
	default:
		return fmt.Sprintf("%d", nTok)
	}
}

func fmtCostShort(usd float64) string {
	if usd >= 100 {
		return fmt.Sprintf("$%.0f", usd)
	}
	return fmt.Sprintf("$%.2f", usd)
}

func fmtDurationShort(secs float64) string {
	switch {
	case secs >= 3600:
		return fmt.Sprintf("%.1fh", secs/3600)
	case secs >= 60:
		return fmt.Sprintf("%.0fm", secs/60)
	default:
		return fmt.Sprintf("%.0fs", secs)
	}
}
