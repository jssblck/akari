package store

import (
	"context"
	"fmt"
	"path"
	"sort"
	"time"
)

// VelocityTrends is the per-bucket cadence: how much hands-on time the fleet logged against
// the wall-clock span it held open, how fast the agent started replying, and how densely
// messages and tools landed. Every series is aligned to the shared bucket grid.
type VelocityTrends struct {
	ActiveHours []float64 // hands-on agent hours per bucket (dead air over the idle gap removed)
	WallHours   []float64 // wall-clock session span hours per bucket
	ResponseP50 []float64 // median prompt-to-first-reply latency, seconds
	ResponseP90 []float64 // 90th percentile, seconds
	ResponseP99 []float64 // 99th percentile, the long tail
	MsgsPerMin  []float64 // messages per active minute
	ToolsPerMin []float64 // tool calls per active minute
}

// ToolPoint is one tool's whole-window reliability reading for the scatter: how much the
// fleet leans on it (calls, and the sessions that used it) against how often it errors.
type ToolPoint struct {
	Name     string
	Calls    int
	Failures int
	Sessions int
	Category string
}

// ErrorRate is the tool's failure share as a percent, 0 when it never ran.
func (t ToolPoint) ErrorRate() float64 {
	if t.Calls == 0 {
		return 0
	}
	return float64(t.Failures) / float64(t.Calls) * 100
}

// ToolFailSeries is one tool's error rate across the bucket grid, for the failure-trend
// lines drawn beside the fleet rate.
type ToolFailSeries struct {
	Name string
	Rate []float64
}

// ToolTrends is the tools read three ways: the whole-window reliability scatter, the
// category mix over time, and the failure rate over time. Reliability is not bucketed (it
// is a snapshot of every tool); Mix and the failure series ride the shared grid.
type ToolTrends struct {
	Reliability []ToolPoint

	MixOrder []string             // category keys, busiest first, "other" last
	Mix      []map[string]float64 // per bucket: category key to percent of the bucket's calls

	FailFleet []float64        // fleet error rate per bucket, percent
	FailWorst []ToolFailSeries // the few worst tools' error rate per bucket
}

// ChurnNode is one row of the file-churn tree: a project, folder, or file, with the edits it
// absorbed and the sessions that returned to it. Project and Folder locate it in the drill
// hierarchy; Path is the full worktree-relative path for a file node.
type ChurnNode struct {
	Project  string
	Folder   string
	Path     string
	Edits    int
	Sessions int
}

// ChurnTrend is the edit-thrash read over time plus the tree the treemap drills: how much
// re-editing happened per bucket and across how many hot files, and the per-file edit
// counts grouped by project and folder. The re-edit figures count only hot files (edited more
// than once across the window), the same set the tree renders, so the headline totals reconcile
// with the file breakdown below them.
type ChurnTrend struct {
	ReEdits       []int // deduped edits of hot files per bucket
	Files         []int // hot files (edited more than once in the window) per bucket
	Tree          []ChurnNode
	Clipped       int // hot files beyond the tree cap, noted rather than shown
	TotalReEdits  int // deduped edits of hot files across the window (sums the tree's edits, before the cap)
	TotalHotFiles int // distinct files re-edited (more than once) across the window
	// Projects is the uncapped count of distinct projects the hot-file cohort spans, before the
	// tree's maxChurnTreeFiles cap. The treemap uses it to tell a genuinely single-project window
	// (root at that project's folders) from a multi-project one whose capped tree happens to show
	// one project (keep the project-level breakdown). Reading the capped tree's project list
	// instead would misjudge a window whose top-N files all sit in one project while a clipped
	// file belongs to another.
	Projects int
}

// GallerySession is one dot in the session gallery: a fully-spanned session placed by how
// long it ran and what it cost, coloured by archetype and carrying its grade and outcome.
type GallerySession struct {
	DurationS      float64
	CostUSD        float64
	CostIncomplete bool // the session's cost rollup folded a token-bearing unpriced event
	Archetype      string
	Grade          string
	Outcome        string
}

// Gallery is the per-session scatter: one point per fully-spanned session in the window
// (capped for the payload), with Total the full count so the panel can note a sample.
type Gallery struct {
	Rows  []GallerySession
	Total int

	// Window-wide summary figures, computed over the full cohort rather than the capped Rows, so
	// the headline medians and the priciest and longest callouts describe every session in the
	// window and not just the most recent maxGalleryPoints kept for the scatter payload.
	MedianDurationS        float64
	MedianCostUSD          float64
	MedianCompletedCostUSD float64
	PriciestDurationS      float64
	PriciestCostUSD        float64
	LongestDurationS       float64
	LongestCostUSD         float64
	// CostIncomplete is true when any session in the cohort folded a token-bearing unpriced
	// event, so the cost summaries (median cost, priciest) are lower bounds.
	CostIncomplete bool
}

// RhythmGrid is the hour-of-week activity heatmap: Cells[dow][hour] is the message-plus-tool
// volume, dow 0 = Monday through 6 = Sunday, hour 0..23 in UTC.
type RhythmGrid struct {
	Cells [][]int
}

// HasData reports whether any cell carried volume.
func (r RhythmGrid) HasData() bool {
	for _, row := range r.Cells {
		for _, v := range row {
			if v > 0 {
				return true
			}
		}
	}
	return false
}

// SubagentStats is the delegation read: how much of the fleet's work runs through subagents,
// how wide the fan-out gets, and the headline figures. DelegateShare and CostShare ride the
// bucket grid; Fanout is the per-bucket spread of how many subagents a delegating session
// spawned.
type SubagentStats struct {
	DelegateShare []float64 // percent of a bucket's root sessions that delegated
	CostShare     []float64 // percent of a bucket's spend that ran through subagents

	FanoutOrder []string         // "one","twoThree","fourSeven","eightPlus"
	FanoutRows  []map[string]int // per bucket: fan-out band to count of delegating sessions

	SessionsThatDelegatePct  float64
	SubagentSessionsInWindow int
	CostThroughSubagentsPct  float64
	DeepestTree              int

	// CostShareIncomplete is true when a token-bearing unpriced event landed on either the
	// subagent numerator or the whole-window denominator, so the cost share is computed from
	// lower-bound dollars and reads as partial (the ratio can move either way).
	CostShareIncomplete bool
}

// HasData reports whether any delegation happened in the window.
func (s SubagentStats) HasData() bool {
	return s.SubagentSessionsInWindow > 0 || s.SessionsThatDelegatePct > 0
}

// velocityTrendsFrom computes the per-bucket velocity series over the velocity rollups:
// latency percentiles from session_turns grouped by the bucket of each turn's prompt,
// throughput from session_activity_hourly, and a wall-clock span sum over sessions for the
// idle-gap read.
func (s *Store) velocityTrendsFrom(ctx context.Context, q querier, f AnalyticsFilter, g trendGrid) (VelocityTrends, error) {
	out := VelocityTrends{
		ActiveHours: make([]float64, g.n()),
		WallHours:   make([]float64, g.n()),
		ResponseP50: make([]float64, g.n()),
		ResponseP90: make([]float64, g.n()),
		ResponseP99: make([]float64, g.n()),
		MsgsPerMin:  make([]float64, g.n()),
		ToolsPerMin: make([]float64, g.n()),
	}

	// Response-time percentiles per bucket, bucketed on the turn's prompt instant.
	filter, args := f.clauseFor("s.started_at")
	rows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT %s AS b,
		        coalesce(percentile_cont(0.5)  WITHIN GROUP (ORDER BY st.response_secs), 0),
		        coalesce(percentile_cont(0.9)  WITHIN GROUP (ORDER BY st.response_secs), 0),
		        coalesce(percentile_cont(0.99) WITHIN GROUP (ORDER BY st.response_secs), 0)
		   FROM session_turns st
		   JOIN sessions s ON s.id = st.session_id
		  WHERE TRUE%s
		  GROUP BY b`, g.sqlBucket("st.prompt_at"), filter), args...)
	if err != nil {
		return VelocityTrends{}, fmt.Errorf("response time trend: %w", err)
	}
	for rows.Next() {
		var b time.Time
		var p50, p90, p99 float64
		if err := rows.Scan(&b, &p50, &p90, &p99); err != nil {
			rows.Close()
			return VelocityTrends{}, fmt.Errorf("scan response time trend: %w", err)
		}
		if i := g.index(b); i >= 0 {
			out.ResponseP50[i], out.ResponseP90[i], out.ResponseP99[i] = p50, p90, p99
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return VelocityTrends{}, fmt.Errorf("iterate response time trend: %w", err)
	}

	// Active time and message and tool volume per bucket, one sum over the activity rollup.
	// The rollup attributed each kept gap to the later message's hour, so re-bucketing its
	// UTC days reproduces the old later-message bucketing.
	filter, args = f.clauseFor("s.started_at")
	arows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT %s AS b,
		        coalesce(sum(ah.active_seconds), 0),
		        coalesce(sum(ah.messages), 0)::bigint,
		        coalesce(sum(ah.tool_calls), 0)::bigint
		   FROM session_activity_hourly ah
		   JOIN sessions s ON s.id = ah.session_id
		  WHERE TRUE%s
		  GROUP BY b`, g.sqlBucketDay("ah.day"), filter), args...)
	if err != nil {
		return VelocityTrends{}, fmt.Errorf("active time trend: %w", err)
	}
	for arows.Next() {
		var b time.Time
		var secs float64
		var nMsgs, nTools int
		if err := arows.Scan(&b, &secs, &nMsgs, &nTools); err != nil {
			arows.Close()
			return VelocityTrends{}, fmt.Errorf("scan active time trend: %w", err)
		}
		if i := g.index(b); i >= 0 {
			out.ActiveHours[i] = secs / 3600
			if secs > 0 {
				mins := secs / 60
				out.MsgsPerMin[i] = float64(nMsgs) / mins
				out.ToolsPerMin[i] = float64(nTools) / mins
			}
		}
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return VelocityTrends{}, fmt.Errorf("iterate active time trend: %w", err)
	}

	// Wall-clock session span per bucket, on the session's start.
	filter, args = f.clauseFor("s.started_at")
	wrows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT %s AS b, coalesce(sum(extract(epoch FROM (s.ended_at - s.started_at))), 0)
		   FROM sessions s
		  WHERE s.started_at IS NOT NULL AND s.ended_at IS NOT NULL AND s.ended_at >= s.started_at%s
		  GROUP BY b`, g.sqlBucket("s.started_at"), filter), args...)
	if err != nil {
		return VelocityTrends{}, fmt.Errorf("wall span trend: %w", err)
	}
	for wrows.Next() {
		var b time.Time
		var secs float64
		if err := wrows.Scan(&b, &secs); err != nil {
			wrows.Close()
			return VelocityTrends{}, fmt.Errorf("scan wall span trend: %w", err)
		}
		if i := g.index(b); i >= 0 {
			out.WallHours[i] = secs / 3600
		}
	}
	wrows.Close()
	if err := wrows.Err(); err != nil {
		return VelocityTrends{}, fmt.Errorf("iterate wall span trend: %w", err)
	}
	return out, nil
}

// maxReliabilityTools caps the reliability scatter at the busiest tools, so a busy fleet stays
// legible instead of crowded with one-off points.
const maxReliabilityTools = 60

// maxMixCategories keeps the busiest tool categories as their own bands, folding the tail
// into "other" so the category-mix stack stays legible.
const maxMixCategories = 6

// toolTrendsFrom computes the reliability scatter (whole window), the category mix per
// bucket, and the failure rate per bucket, all over the session_tool_rollup rows of
// started_at-windowed sessions (the rollup deduped replays at write time, the same way the
// headline tool stats read), bucketing on the session's start.
func (s *Store) toolTrendsFrom(ctx context.Context, q querier, f AnalyticsFilter, g trendGrid) (ToolTrends, error) {
	var out ToolTrends

	// Reliability: every tool's calls / failures / sessions / category over the window.
	filter, args := f.clauseFor("s.started_at")
	limitArg := fmt.Sprintf("$%d", len(args)+1)
	args = append(args, maxReliabilityTools)
	rows, err := q.Query(ctx,
		`SELECT tr.tool_name, min(tr.category) AS cat,
		        sum(tr.calls)::bigint AS calls,
		        sum(tr.failures)::bigint AS failures,
		        count(DISTINCT tr.session_id) AS sessions
		   FROM session_tool_rollup tr
		   JOIN sessions s ON s.id = tr.session_id
		  WHERE TRUE`+filter+`
		  GROUP BY tr.tool_name
		  ORDER BY calls DESC, tr.tool_name
		  LIMIT `+limitArg, args...)
	if err != nil {
		return ToolTrends{}, fmt.Errorf("tool reliability: %w", err)
	}
	for rows.Next() {
		var t ToolPoint
		if err := rows.Scan(&t.Name, &t.Category, &t.Calls, &t.Failures, &t.Sessions); err != nil {
			rows.Close()
			return ToolTrends{}, fmt.Errorf("scan tool reliability: %w", err)
		}
		out.Reliability = append(out.Reliability, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ToolTrends{}, fmt.Errorf("iterate tool reliability: %w", err)
	}

	// Category mix per bucket, from the same rollup base, bucketed on the session start.
	filter, args = f.clauseFor("s.started_at")
	mrows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT %s AS b, tr.category, sum(tr.calls)::bigint
		   FROM session_tool_rollup tr
		   JOIN sessions s ON s.id = tr.session_id
		  WHERE TRUE%s
		  GROUP BY 1, 2`,
		g.sqlBucket("s.started_at"), filter), args...)
	if err != nil {
		return ToolTrends{}, fmt.Errorf("tool mix trend: %w", err)
	}
	perBucketCat := make([]map[string]int, g.n())
	for i := range perBucketCat {
		perBucketCat[i] = map[string]int{}
	}
	catTotal := map[string]int{}
	for mrows.Next() {
		var b time.Time
		var cat string
		var n int
		if err := mrows.Scan(&b, &cat, &n); err != nil {
			mrows.Close()
			return ToolTrends{}, fmt.Errorf("scan tool mix trend: %w", err)
		}
		if i := g.index(b); i >= 0 {
			perBucketCat[i][cat] += n
			catTotal[cat] += n
		}
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return ToolTrends{}, fmt.Errorf("iterate tool mix trend: %w", err)
	}
	out.MixOrder, out.Mix = foldCategoryMix(perBucketCat, catTotal)

	// Failures: the fleet error rate per bucket plus the few worst tools' rate.
	if err := s.toolFailureTrend(ctx, q, f, g, &out); err != nil {
		return ToolTrends{}, err
	}
	return out, nil
}

// foldCategoryMix ranks categories by total calls, keeps the busiest as their own bands and
// folds the rest into "other", then normalizes each bucket to percent.
func foldCategoryMix(perBucket []map[string]int, total map[string]int) ([]string, []map[string]float64) {
	cats := make([]string, 0, len(total))
	for c := range total {
		cats = append(cats, c)
	}
	sort.Slice(cats, func(a, b int) bool {
		if total[cats[a]] != total[cats[b]] {
			return total[cats[a]] > total[cats[b]]
		}
		return cats[a] < cats[b]
	})
	keep := map[string]bool{}
	order := []string{}
	foldOther := false
	for i, c := range cats {
		if i < maxMixCategories && c != "other" {
			keep[c] = true
			order = append(order, c)
		} else {
			foldOther = true
		}
	}
	if foldOther || keep["other"] {
		order = append(order, "other")
	}
	mix := make([]map[string]float64, len(perBucket))
	for i, counts := range perBucket {
		row := map[string]float64{}
		var sum int
		folded := map[string]int{}
		for c, n := range counts {
			if keep[c] {
				folded[c] += n
			} else {
				folded["other"] += n
			}
			sum += n
		}
		if sum > 0 {
			for c, n := range folded {
				row[c] = float64(n) / float64(sum) * 100
			}
		}
		mix[i] = row
	}
	return order, mix
}

// maxFailTools is how many of the highest-failure tools get their own line beside the fleet
// rate, so the failure trend names the worst offenders without crowding the chart.
const maxFailTools = 3

// toolFailureTrend fills the fleet error rate per bucket and the worst tools' per-bucket
// rate. It picks the worst tools from the reliability scan (highest failures among tools
// with enough volume to read), then scans their per-bucket calls and failures.
func (s *Store) toolFailureTrend(ctx context.Context, q querier, f AnalyticsFilter, g trendGrid, out *ToolTrends) error {
	out.FailFleet = make([]float64, g.n())

	// Fleet rate per bucket over the rollup base.
	filter, args := f.clauseFor("s.started_at")
	rows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT %s AS b, sum(tr.calls)::bigint, sum(tr.failures)::bigint
		   FROM session_tool_rollup tr
		   JOIN sessions s ON s.id = tr.session_id
		  WHERE TRUE%s
		  GROUP BY 1`, g.sqlBucket("s.started_at"), filter), args...)
	if err != nil {
		return fmt.Errorf("tool failure fleet trend: %w", err)
	}
	for rows.Next() {
		var b time.Time
		var calls, fails int
		if err := rows.Scan(&b, &calls, &fails); err != nil {
			rows.Close()
			return fmt.Errorf("scan tool failure fleet trend: %w", err)
		}
		if i := g.index(b); i >= 0 && calls > 0 {
			out.FailFleet[i] = float64(fails) / float64(calls) * 100
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate tool failure fleet trend: %w", err)
	}

	// Pick the worst tools: highest failure count among tools with enough calls to read a
	// rate off. Reliability is already sorted by calls; sort a copy by failures.
	worst := make([]ToolPoint, 0, len(out.Reliability))
	for _, t := range out.Reliability {
		if t.Calls >= 50 && t.Failures > 0 {
			worst = append(worst, t)
		}
	}
	sort.Slice(worst, func(a, b int) bool { return worst[a].Failures > worst[b].Failures })
	if len(worst) > maxFailTools {
		worst = worst[:maxFailTools]
	}
	if len(worst) == 0 {
		return nil
	}
	names := make([]string, len(worst))
	for i, t := range worst {
		names[i] = t.Name
	}

	filter, args = f.clauseFor("s.started_at")
	args = append(args, names)
	nameArg := fmt.Sprintf("$%d", len(args))
	frows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT tr.tool_name, %s AS b, sum(tr.calls)::bigint, sum(tr.failures)::bigint
		   FROM session_tool_rollup tr
		   JOIN sessions s ON s.id = tr.session_id
		  WHERE tr.tool_name = ANY(%s)%s
		  GROUP BY tr.tool_name, b`,
		g.sqlBucket("s.started_at"), nameArg, filter), args...)
	if err != nil {
		return fmt.Errorf("tool failure worst trend: %w", err)
	}
	rateByTool := map[string][]float64{}
	for _, n := range names {
		rateByTool[n] = make([]float64, g.n())
	}
	for frows.Next() {
		var name string
		var b time.Time
		var calls, fails int
		if err := frows.Scan(&name, &b, &calls, &fails); err != nil {
			frows.Close()
			return fmt.Errorf("scan tool failure worst trend: %w", err)
		}
		if i := g.index(b); i >= 0 && calls > 0 {
			rateByTool[name][i] = float64(fails) / float64(calls) * 100
		}
	}
	frows.Close()
	if err := frows.Err(); err != nil {
		return fmt.Errorf("iterate tool failure worst trend: %w", err)
	}
	for _, n := range names {
		out.FailWorst = append(out.FailWorst, ToolFailSeries{Name: n, Rate: rateByTool[n]})
	}
	return nil
}

// maxChurnTreeFiles caps the churn tree at the busiest files, so the treemap shows where the
// thrash concentrates instead of fragmenting into one-off touches.
const maxChurnTreeFiles = 150

// churnTrendFrom computes the per-bucket hot-file re-edit volume and hot-file count, plus the
// project/folder/file tree the treemap drills, over the session_file_churn rollup (edits
// deduped at write time), bucketing on the session start; both the trend and the tree count
// only files edited more than once, so the headline re-edit total reconciles with the file
// breakdown the tree renders.
func (s *Store) churnTrendFrom(ctx context.Context, q querier, f AnalyticsFilter, g trendGrid) (ChurnTrend, error) {
	out := ChurnTrend{ReEdits: make([]int, g.n()), Files: make([]int, g.n())}

	// Per-bucket re-edit volume and hot-file count. A file is hot when it is edited more than once
	// across the whole window (the same definition the tree and TotalHotFiles use), and it counts
	// in each bucket it was edited in. Defining hot per bucket instead would drop a file edited
	// once in each of two buckets: hot in the window total, invisible in every bucket. The totals
	// CTE re-sums each file's per-bucket edits into its window total, then the final grouping
	// counts, per bucket, the files whose window total clears the bar. Both the edit sum and the
	// file count filter on that same hot predicate, so ReEdits/TotalReEdits count only edits of
	// re-edited files and reconcile with the tree the treemap renders (whose files are the same
	// HAVING sum(edits) > 1 set); a one-off edit to a unique file lands in neither.
	filter, args := f.clauseFor("s.started_at")
	rows, err := q.Query(ctx, fmt.Sprintf(
		`WITH perfile AS (
		   SELECT %s AS b, s.project_id, fc.churn_path, sum(fc.edits) AS edits_in_bucket
		     FROM session_file_churn fc
		     JOIN sessions s ON s.id = fc.session_id
		    WHERE TRUE%s
		    GROUP BY 1, s.project_id, fc.churn_path
		 ),
		 totals AS (
		   SELECT project_id, churn_path, sum(edits_in_bucket) AS edits_total
		     FROM perfile GROUP BY project_id, churn_path
		 )
		 SELECT p.b,
		        coalesce(sum(p.edits_in_bucket) FILTER (WHERE t.edits_total > 1), 0)::bigint,
		        count(*) FILTER (WHERE t.edits_total > 1)
		   FROM perfile p JOIN totals t USING (project_id, churn_path)
		  GROUP BY p.b`, g.sqlBucket("s.started_at"), filter), args...)
	if err != nil {
		return ChurnTrend{}, fmt.Errorf("churn trend: %w", err)
	}
	for rows.Next() {
		var b time.Time
		var edits, files int
		if err := rows.Scan(&b, &edits, &files); err != nil {
			rows.Close()
			return ChurnTrend{}, fmt.Errorf("scan churn trend: %w", err)
		}
		if i := g.index(b); i >= 0 {
			out.ReEdits[i] = edits
			out.Files[i] = files
			out.TotalReEdits += edits
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ChurnTrend{}, fmt.Errorf("iterate churn trend: %w", err)
	}

	// The tree: the busiest churned files with project label and folder.
	filter, args = f.clauseFor("s.started_at")
	limitArg := fmt.Sprintf("$%d", len(args)+1)
	args = append(args, maxChurnTreeFiles)
	trows, err := q.Query(ctx,
		`WITH agg AS (
		   SELECT s.project_id, fc.churn_path,
		          sum(fc.edits)::bigint AS edits,
		          count(DISTINCT fc.session_id) AS sessions
		     FROM session_file_churn fc
		     JOIN sessions s ON s.id = fc.session_id
		    WHERE TRUE`+filter+`
		    GROUP BY s.project_id, fc.churn_path
		   HAVING sum(fc.edits) > 1
		 )
		 SELECT CASE WHEN p.kind IN ('standalone', 'orphaned') THEN p.display_name ELSE p.remote_key END AS project,
		        agg.churn_path, agg.edits, agg.sessions,
		        (SELECT count(*) FROM agg) AS total,
		        (SELECT count(DISTINCT project_id) FROM agg) AS proj_total
		   FROM agg JOIN projects p ON p.id = agg.project_id
		  ORDER BY agg.edits DESC, project, agg.churn_path
		  LIMIT `+limitArg, args...)
	if err != nil {
		return ChurnTrend{}, fmt.Errorf("churn tree: %w", err)
	}
	var total, projTotal int
	for trows.Next() {
		var n ChurnNode
		if err := trows.Scan(&n.Project, &n.Path, &n.Edits, &n.Sessions, &total, &projTotal); err != nil {
			trows.Close()
			return ChurnTrend{}, fmt.Errorf("scan churn tree: %w", err)
		}
		n.Folder = churnFolder(n.Path)
		out.Tree = append(out.Tree, n)
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return ChurnTrend{}, fmt.Errorf("iterate churn tree: %w", err)
	}
	out.TotalHotFiles = total
	// The uncapped project span of the hot-file cohort. Both totals ride every tree row (correlated
	// subqueries over the same agg CTE), so an empty tree leaves them zero, which reads as no churn.
	out.Projects = projTotal
	if total > len(out.Tree) {
		out.Clipped = total - len(out.Tree)
	}
	return out, nil
}

// churnFolder derives the treemap's middle drill level from a file path: its directory, or
// "(root)" for a top-level file, so the tree groups project to folder to file.
func churnFolder(p string) string {
	dir := path.Dir(p)
	if dir == "." || dir == "/" || dir == "" {
		return "(root)"
	}
	return dir
}

// maxGalleryPoints caps the session scatter so a big window ships a bounded payload; the
// panel notes the sample when Total exceeds it. The most recent sessions are kept.
const maxGalleryPoints = 400

// galleryFrom reads one point per fully-spanned session in the window: its duration, cost,
// archetype, and gated grade and outcome. Total is the full count so the panel can note a
// sample when the window holds more than the cap.
func (s *Store) galleryFrom(ctx context.Context, q querier, f AnalyticsFilter) (Gallery, error) {
	var out Gallery
	filter, args := f.clauseFor("s.started_at")
	limitArg := fmt.Sprintf("$%d", len(args)+1)
	args = append(args, maxGalleryPoints)
	rows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT extract(epoch FROM (s.ended_at - s.started_at)) AS dur,
		        s.total_cost_usd,
		        s.cost_incomplete,
		        %s AS archetype,
		        coalesce(sig.grade, '') AS grade,
		        coalesce(sig.outcome, 'unknown') AS outcome,
		        count(*) OVER () AS total
		   FROM sessions s
		   LEFT JOIN session_signals sig
		     ON sig.session_id = s.id AND `+signalsCurrent()+`
		  WHERE s.started_at IS NOT NULL AND s.ended_at IS NOT NULL AND s.ended_at >= s.started_at%s
		  ORDER BY s.started_at DESC
		  LIMIT %s`, archetypeCaseExpr, filter, limitArg), args...)
	if err != nil {
		return Gallery{}, fmt.Errorf("session gallery: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var gs GallerySession
		var total int
		if err := rows.Scan(&gs.DurationS, &gs.CostUSD, &gs.CostIncomplete, &gs.Archetype, &gs.Grade, &gs.Outcome, &total); err != nil {
			return Gallery{}, fmt.Errorf("scan session gallery: %w", err)
		}
		out.Rows = append(out.Rows, gs)
		out.Total = total
	}
	if err := rows.Err(); err != nil {
		return Gallery{}, fmt.Errorf("iterate session gallery: %w", err)
	}

	// Window-wide summaries over the full cohort, so the headline medians and the priciest and
	// longest callouts are not skewed by the maxGalleryPoints sampling that bounds Rows. The two
	// max(ARRAY[...]) pairs pick the priciest (by cost, ties by duration) and longest (by
	// duration, ties by cost) session in a single pass: max over a numeric array compares
	// element by element, so the leading key wins and the trailing key breaks ties.
	sfilter, sargs := f.clauseFor("s.started_at")
	var medDur, medCost, medComp float64
	var priciest, longest []float64
	if err := q.QueryRow(ctx, fmt.Sprintf(
		`WITH cohort AS (
		   SELECT extract(epoch FROM (s.ended_at - s.started_at)) AS dur,
		          s.total_cost_usd AS cost,
		          s.cost_incomplete AS cost_incomplete,
		          coalesce(sig.outcome, 'unknown') AS outcome
		     FROM sessions s
		     LEFT JOIN session_signals sig
		       ON sig.session_id = s.id AND `+signalsCurrent()+`
		    WHERE s.started_at IS NOT NULL AND s.ended_at IS NOT NULL AND s.ended_at >= s.started_at%s
		 )
		 SELECT coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY dur), 0),
		        coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY cost), 0),
		        coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY cost) FILTER (WHERE outcome = 'completed'), 0),
		        max(ARRAY[cost, dur]),
		        max(ARRAY[dur, cost]),
		        coalesce(bool_or(cost_incomplete), false)
		   FROM cohort`, sfilter), sargs...).
		Scan(&medDur, &medCost, &medComp, &priciest, &longest, &out.CostIncomplete); err != nil {
		return Gallery{}, fmt.Errorf("session gallery summary: %w", err)
	}
	out.MedianDurationS = medDur
	out.MedianCostUSD = medCost
	out.MedianCompletedCostUSD = medComp
	if len(priciest) == 2 {
		out.PriciestCostUSD, out.PriciestDurationS = priciest[0], priciest[1]
	}
	if len(longest) == 2 {
		out.LongestDurationS, out.LongestCostUSD = longest[0], longest[1]
	}
	return out, nil
}

// rhythmFrom builds the hour-of-week activity grid: message plus tool volume per (day of
// week, hour), in UTC, over the scoped sessions. Rows are Monday-first to match the concept's
// punchcard.
func (s *Store) rhythmFrom(ctx context.Context, q querier, f AnalyticsFilter) (RhythmGrid, error) {
	cells := make([][]int, 7)
	for i := range cells {
		cells[i] = make([]int, 24)
	}
	add := func(dow, hour, n int) {
		if dow >= 1 && dow <= 7 && hour >= 0 && hour <= 23 {
			cells[dow-1][hour] += n // isodow: 1=Mon..7=Sun -> index 0..6
		}
	}

	// The activity rollup carries both volumes per UTC (day, hour); the day of week derives
	// from the stored day, so the punchcard is one indexed read instead of two message scans.
	filter, args := f.clauseFor("s.started_at")
	mrows, err := q.Query(ctx,
		`SELECT extract(isodow FROM ah.day)::int,
		        ah.hour::int,
		        sum(ah.messages + ah.tool_calls)::bigint
		   FROM session_activity_hourly ah
		   JOIN sessions s ON s.id = ah.session_id
		  WHERE TRUE`+filter+`
		  GROUP BY 1, 2`, args...)
	if err != nil {
		return RhythmGrid{}, fmt.Errorf("rhythm activity: %w", err)
	}
	for mrows.Next() {
		var dow, hour, n int
		if err := mrows.Scan(&dow, &hour, &n); err != nil {
			mrows.Close()
			return RhythmGrid{}, fmt.Errorf("scan rhythm activity: %w", err)
		}
		add(dow, hour, n)
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return RhythmGrid{}, fmt.Errorf("iterate rhythm activity: %w", err)
	}
	return RhythmGrid{Cells: cells}, nil
}

// subagentTrendsFrom computes the delegation picture: per bucket how many root sessions
// delegated and how wide they fanned out, what share of spend ran through subagents, and the
// headline figures. Root sessions are the non-subagent, non-continuation sessions; a
// delegating root is one with at least one subagent child.
func (s *Store) subagentTrendsFrom(ctx context.Context, q querier, f AnalyticsFilter, g trendGrid) (SubagentStats, error) {
	out := SubagentStats{
		DelegateShare: make([]float64, g.n()),
		CostShare:     make([]float64, g.n()),
		FanoutOrder:   []string{"one", "twoThree", "fourSeven", "eightPlus"},
		FanoutRows:    make([]map[string]int, g.n()),
	}
	for i := range out.FanoutRows {
		out.FanoutRows[i] = map[string]int{}
	}

	// Per-bucket root counts, delegating roots, and the fan-out spread. The children are
	// joined to the scoped roots (not grouped across the whole sessions table), so a 7-day or
	// per-project request only touches children of in-window roots. The child join carries no
	// window of its own, so a child that ran outside the window still counts toward its root's
	// fan-out.
	filter, args := f.clauseFor("s.started_at")
	rows, err := q.Query(ctx, fmt.Sprintf(
		`WITH roots AS (
		   SELECT s.id, %s AS b
		     FROM sessions s
		    WHERE s.started_at IS NOT NULL AND s.relationship_type = ''%s
		 ),
		 kids AS (
		   SELECT r.id, r.b, count(c.id) AS n
		     FROM roots r
		     LEFT JOIN sessions c
		       ON c.parent_session_id = r.id AND c.relationship_type = 'subagent'
		    GROUP BY r.id, r.b
		 )
		 SELECT b,
		        count(*) AS roots,
		        count(*) FILTER (WHERE n >= 1) AS delegating,
		        count(*) FILTER (WHERE n = 1) AS f1,
		        count(*) FILTER (WHERE n BETWEEN 2 AND 3) AS f23,
		        count(*) FILTER (WHERE n BETWEEN 4 AND 7) AS f47,
		        count(*) FILTER (WHERE n >= 8) AS f8
		   FROM kids
		  GROUP BY b`, g.sqlBucket("s.started_at"), filter), args...)
	if err != nil {
		return SubagentStats{}, fmt.Errorf("subagent delegation trend: %w", err)
	}
	var totalRoots, totalDelegating int
	for rows.Next() {
		var b time.Time
		var roots, delegating, f1, f23, f47, f8 int
		if err := rows.Scan(&b, &roots, &delegating, &f1, &f23, &f47, &f8); err != nil {
			rows.Close()
			return SubagentStats{}, fmt.Errorf("scan subagent delegation trend: %w", err)
		}
		totalRoots += roots
		totalDelegating += delegating
		if i := g.index(b); i >= 0 {
			if roots > 0 {
				out.DelegateShare[i] = float64(delegating) / float64(roots) * 100
			}
			out.FanoutRows[i] = map[string]int{"one": f1, "twoThree": f23, "fourSeven": f47, "eightPlus": f8}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return SubagentStats{}, fmt.Errorf("iterate subagent delegation trend: %w", err)
	}
	if totalRoots > 0 {
		out.SessionsThatDelegatePct = float64(totalDelegating) / float64(totalRoots) * 100
	}

	// Cost share per bucket: subagent spend over total spend, on the usage rollup's UTC day.
	filter, args = f.clauseForRollupDay("sud.day")
	var totalCost, subCost float64
	crows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT %s AS b,
		        coalesce(sum(sud.cost_usd), 0),
		        coalesce(sum(sud.cost_usd) FILTER (WHERE s.relationship_type = 'subagent'), 0),
		        coalesce(bool_or(sud.unpriced), false)
		   FROM session_usage_daily sud
		   JOIN sessions s ON s.id = sud.session_id
		  WHERE sud.day IS NOT NULL%s
		  GROUP BY b`, g.sqlBucketDay("sud.day"), filter), args...)
	if err != nil {
		return SubagentStats{}, fmt.Errorf("subagent cost trend: %w", err)
	}
	for crows.Next() {
		var b time.Time
		var total, sub float64
		var incomplete bool
		if err := crows.Scan(&b, &total, &sub, &incomplete); err != nil {
			crows.Close()
			return SubagentStats{}, fmt.Errorf("scan subagent cost trend: %w", err)
		}
		totalCost += total
		subCost += sub
		out.CostShareIncomplete = out.CostShareIncomplete || incomplete
		if i := g.index(b); i >= 0 && total > 0 {
			out.CostShare[i] = sub / total * 100
		}
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return SubagentStats{}, fmt.Errorf("iterate subagent cost trend: %w", err)
	}
	if totalCost > 0 {
		out.CostThroughSubagentsPct = subCost / totalCost * 100
	}

	// Subagent session count and deepest tree, over the window.
	filter, args = f.clauseFor("s.started_at")
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM sessions s WHERE s.relationship_type = 'subagent'`+filter, args...).
		Scan(&out.SubagentSessionsInWindow); err != nil {
		return SubagentStats{}, fmt.Errorf("subagent session count: %w", err)
	}

	filter, args = f.clauseFor("s.started_at")
	if err := q.QueryRow(ctx,
		`WITH RECURSIVE t AS (
		   SELECT s.id, 1 AS depth FROM sessions s WHERE s.relationship_type = ''`+filter+`
		   UNION ALL
		   SELECT c.id, t.depth + 1 FROM sessions c JOIN t ON c.parent_session_id = t.id
		 )
		 SELECT coalesce(max(depth), 0) FROM t`, args...).Scan(&out.DeepestTree); err != nil {
		return SubagentStats{}, fmt.Errorf("subagent deepest tree: %w", err)
	}
	return out, nil
}
