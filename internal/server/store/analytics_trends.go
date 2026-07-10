package store

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jssblck/akari/internal/pricing"
	"github.com/jssblck/akari/internal/quality"
)

// Trends is the time-bucketed counterpart to the Insights distributions: the same scoped
// cohort read as a grid of buckets (days or weeks, per AnalyticsFilter.Bucket) so the page
// can draw how the fleet moved over the window rather than one rolled-up number. Every
// per-bucket series is indexed against BucketStarts (oldest first), zero-filled so an empty
// week still draws a point and the range selector windows every chart together. It is
// computed only when the filter names a bucket; the distributions-only callers leave it nil.
type Trends struct {
	Unit         string      // "day" or "week", the bucket width every series shares
	BucketStarts []time.Time // bucket start instants, oldest to newest; the x axis
	Labels       []string    // formatted bucket labels, aligned to BucketStarts

	FleetMix  FleetMix       // token share by model per bucket
	Gallery   Gallery        // one point per fully-spanned session (duration x cost)
	Velocity  VelocityTrends // active hours, response latency, throughput per bucket
	Tools     ToolTrends     // reliability scatter, category mix, failure rate per bucket
	Churn     ChurnTrend     // re-edit trend plus the project/folder/file tree
	Signals   SignalTrends   // grades, outcomes, hygiene, context per bucket
	Economics Economics      // cost of quality and cache savings per bucket
	Subagents SubagentStats  // delegation share and fan-out per bucket
	Rhythm    RhythmGrid     // message + tool volume by hour of week
}

// HasData reports whether the trend grid carries any buckets to draw.
func (t *Trends) HasData() bool { return t != nil && len(t.BucketStarts) > 0 }

// ModelSeries is one model's token share across the bucket grid: Share[i] is the model's
// percent of bucket i's total tokens, and First is the first bucket index it appears in
// (so a model that arrived mid-window draws a line only from its arrival, not a flat zero
// run before it existed). Models are ordered by total tokens descending, with an "other"
// fold of the long tail last.
type ModelSeries struct {
	Model string
	Share []float64
	First int
}

// FleetMix is the per-bucket token share by model, the stacked-area read of a model
// migration: a new model eating an incumbent's share shows up here as one band growing as
// another shrinks, without reading release notes.
type FleetMix struct {
	Models []ModelSeries

	// NewestModel and NewestFirst name the newest arrival: the scoped model whose
	// first tokens EVER land latest, provided that first day falls inside this grid
	// past its first bucket. Arrival is a fleet-history fact, not a window-relative
	// one, and it is computed over every model, not just the kept bands. Both halves
	// of that matter: a window-relative "first bucket with tokens" would call any
	// incumbent with a quiet first day an arrival on a short window, and the top-N
	// fold (ranked by whole-window tokens) buries a true arrival in "other" on a long
	// one; either way two windows would name different "newest" models over the same
	// corpus. NewestModel is empty (NewestFirst -1) when nothing arrived inside the
	// window: incumbents predate it, and "unknown" usage is skipped because a batch
	// of unattributed tokens is not a model's story.
	NewestModel string
	NewestFirst int
}

// HasData reports whether any model carried tokens in the window.
func (f FleetMix) HasData() bool { return len(f.Models) > 0 }

// SignalTrends is the per-bucket read of the settle-pass signals: how grades, outcomes,
// prompt hygiene, and context resets moved over the window. Every series is gated the same
// way the distributions are (current signals version, not stale), so an ungraded bucket
// reads as unscored rather than zero.
type SignalTrends struct {
	// Grades holds the per-bucket grade share. GradeShare[i] maps a grade key
	// (A/B/C/D/F/"" for unscored) to its percent of bucket i's sessions.
	GradeShare []map[string]float64
	GPA        []float64 // grade-point average per bucket over the graded sessions, 0..4

	// ArchetypeShare holds the per-bucket archetype mix. ArchetypeShare[i] maps an archetype
	// key (quick/standard/deep/marathon/automation) to its percent of bucket i's sessions,
	// banded by the same archetypeCaseExpr the distribution uses so the trend and the rolled-up
	// mix read one numeric source. Unlike the grade and outcome series it needs no signals join:
	// a session's shape is a fact of its own columns (user turns, message count, span), so an
	// ungraded session still bands, and the denominator is every started session in the bucket.
	ArchetypeShare []map[string]float64

	CompletedRate []float64 // percent of bucket i's sessions that completed
	AbandonedRate []float64 // percent that abandoned
	OutcomeTotal  []int     // sessions in bucket i (the rate denominator)
	// Raw per-bucket outcome counts behind the rates, so the outcome chart's magnitude bars draw
	// the store's completed/abandoned/other partition exactly rather than deriving a warn segment
	// as total-completed, which folds errored and unknown into the abandoned colour and drifts
	// from the abandoned-rate line. Other is OutcomeTotal minus these two.
	CompletedCount []int
	AbandonedCount []int

	// Hygiene rates, each a percent of the bucket's prompts (or sessions, for
	// unstructured starts), gated on the current prompt-facts version.
	HygieneTerse        []float64
	HygieneRepeated     []float64
	HygieneNoCode       []float64
	HygieneUnstructured []float64

	ContextResets []int // inferred context resets summed per bucket

	// ContextHistogram is the window-wide distribution of per-session peak context, a
	// log-scale histogram (not per bucket). Markers carries the p50/p90/max annotations.
	ContextHistogram []ContextBucket
	ContextMarkers   []ContextMarker
}

// ContextBucket is one log-scale bin of the peak-context histogram: [Lo, Hi) tokens and how
// many sessions peaked inside it.
type ContextBucket struct {
	Lo    int64
	Hi    int64
	Count int
}

// ContextMarker annotates the histogram with an order statistic at a token position.
// Kind names the statistic ("p50", "p90", "max"); the axis label is formatted at render
// time in web, so the store carries no presentation strings.
type ContextMarker struct {
	Tokens int64
	Kind   string
}

// Economics is the per-bucket money read: spend split by outcome class (completed, abandoned,
// and everything else) so the three bands sum to the window's total spend and the abandon rate
// carries a dollar figure, plus what caching saved.
type Economics struct {
	CostCompleted []float64 // dollars spent in bucket i by sessions that completed
	CostAbandoned []float64 // dollars spent in bucket i by sessions that abandoned (outcome='abandoned')
	CostOther     []float64 // dollars spent in bucket i by every other outcome (errored, unknown, ungraded)
	CacheSavings  []float64 // dollars caching saved in bucket i, priced per day and model
	CacheHitRate  []float64 // cache-read share of all prompt-side tokens (input+read+write), percent
	CacheMeasured []bool    // whether bucket i had prompt-side tokens, so a 0 rate reads as measured 0% not "no data"

	TotalSpend         float64 // all spend across the window, every outcome
	TotalAbandoned     float64 // spend by abandoned sessions across the window
	AbandonedSharePct  float64
	TotalCacheSavings  float64
	CacheHitRateLatest float64 // the latest measured bucket's hit rate (a real 0% included), 0 when no bucket was measured

	// CostIncomplete is true when the window folded in a token-bearing usage event with no
	// price, so every spend figure here is a lower bound, the same flag Analytics carries.
	CostIncomplete bool
	// AbandonedIncomplete is CostIncomplete narrowed to the abandoned subset: true when an
	// abandoned session's usage carried token volume with no price, so TotalAbandoned alone is a
	// lower bound. It is separate from CostIncomplete because a window can be incomplete on its
	// completed spend while its abandoned spend is fully priced (or the reverse), so the
	// abandoned subfigure must carry its own marker rather than the whole window's.
	AbandonedIncomplete bool
	// CacheSavingsIncomplete is true when cached read or write volume rode a model the pricing
	// table cannot price, so the savings total omits it. The omitted term can be either sign, so
	// this is "partial", not a lower bound, matching CacheStats.SavingsIncomplete.
	CacheSavingsIncomplete bool
}

// trendGrid is the shared bucket spine every trend series projects onto: the ordered bucket
// starts plus an index from a truncated instant back to its position, so a GROUP BY
// date_trunc scan can be zero-filled onto a continuous grid.
type trendGrid struct {
	Unit   string
	Starts []time.Time
	idx    map[time.Time]int
}

// truncBucket truncates an instant to its bucket start in UTC, matching Postgres
// date_trunc(unit, ts AT TIME ZONE 'UTC'): weeks start Monday, days at midnight UTC. The
// SQL and this Go must agree so a scanned bucket start lands on the grid it was generated
// from.
func truncBucket(unit string, t time.Time) time.Time {
	t = t.UTC()
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	if unit == "week" {
		// Postgres date_trunc('week') anchors on Monday; Go Weekday has Sunday=0.
		delta := (int(t.Weekday()) + 6) % 7
		return day.AddDate(0, 0, -delta)
	}
	return day
}

func advanceBucket(unit string, t time.Time) time.Time {
	if unit == "week" {
		return t.AddDate(0, 0, 7)
	}
	return t.AddDate(0, 0, 1)
}

// retreatBuckets steps back n whole buckets from t, the inverse of applying advanceBucket n
// times. Buckets are a fixed width in UTC (a day is 24h, a week is 7 days, with no DST to
// stretch them), so subtracting n spans lands on the same bucket start advanceBucket would
// have reached going forward, without the loop.
func retreatBuckets(unit string, t time.Time, n int) time.Time {
	if unit == "week" {
		return t.AddDate(0, 0, -7*n)
	}
	return t.AddDate(0, 0, -n)
}

// maxTrendBuckets caps the grid so an unbounded "all" window still renders a readable,
// bounded payload: past the cap the window is trimmed to the most recent buckets rather
// than streaming years of weeks the chart could not show.
const maxTrendBuckets = 120

// newTrendGrid builds the bucket spine spanning [since, until] at the given unit. A zero
// since (the "all" window) is caller-resolved to the earliest session start before this is
// called, so the grid is always bounded.
func newTrendGrid(unit string, since, until time.Time) trendGrid {
	if unit != "week" {
		unit = "day"
	}
	start := truncBucket(unit, since)
	end := truncBucket(unit, until)
	if end.Before(start) {
		end = start
	}
	// Cap the start to the last maxTrendBuckets ending at end BEFORE building the spine, so an
	// "all" window whose earliest session is years back does not allocate and walk one bucket per
	// day of that history only to trim all but the final 120 away. Request time and peak memory
	// then track the rendered span, not the corpus age. Retreating maxTrendBuckets-1 whole buckets
	// from end lands the exact first bucket the old trailing slice produced (the buckets are a
	// fixed width, so the step is exact), so the grid is unchanged, only cheaper to build.
	if capped := retreatBuckets(unit, end, maxTrendBuckets-1); capped.After(start) {
		start = capped
	}
	var starts []time.Time
	for b := start; !b.After(end); b = advanceBucket(unit, b) {
		starts = append(starts, b)
	}
	idx := make(map[time.Time]int, len(starts))
	for i, b := range starts {
		idx[b] = i
	}
	return trendGrid{Unit: unit, Starts: starts, idx: idx}
}

// index maps a scanned bucket start to its grid position, or -1 if it falls outside the
// grid (an event just past a bound). Callers skip a -1 rather than fold it into an edge.
func (g trendGrid) index(t time.Time) int {
	if i, ok := g.idx[truncBucket(g.Unit, t)]; ok {
		return i
	}
	return -1
}

func (g trendGrid) n() int { return len(g.Starts) }

// labels formats each bucket start for the x axis. Both units label with the bucket's start
// date ("Jan 2"); the unit distinguishes them in the caption, not the tick.
func (g trendGrid) labels() []string {
	out := make([]string, len(g.Starts))
	for i, b := range g.Starts {
		out[i] = b.Format("Jan 2")
	}
	return out
}

// sqlBucket is the date_trunc expression for a timestamp column at this grid's unit, in
// UTC so the buckets align with the Go-side grid. The unit is one of our own two constants,
// never caller input, so interpolating it is safe (the same latitude clauseFor takes with
// its time expression).
func (g trendGrid) sqlBucket(col string) string {
	return fmt.Sprintf("date_trunc('%s', %s AT TIME ZONE 'UTC')", g.Unit, col)
}

// sqlBucketDay is sqlBucket for a rollup's UTC DATE column: the date is already the UTC
// day, so it truncates the plain cast rather than shifting a timestamptz. Day sums roll up
// exactly onto the week grid (date_trunc('week') of a date's midnight is the same Monday
// the source instant truncated to).
func (g trendGrid) sqlBucketDay(col string) string {
	return fmt.Sprintf("date_trunc('%s', %s::timestamp)", g.Unit, col)
}

// resolveTrendSince returns the effective lower bound for the grid: the filter's Since when
// set, else the earliest scoped session start (the "all" window), else a short fallback so
// an empty corpus still yields a one-bucket grid rather than an empty one.
func (s *Store) resolveTrendSince(ctx context.Context, q querier, f AnalyticsFilter, now time.Time) (time.Time, error) {
	if !f.Since.IsZero() {
		return f.Since, nil
	}
	filter, args := f.clauseFor("s.started_at")
	var earliest *time.Time
	if err := q.QueryRow(ctx,
		`SELECT min(s.started_at) FROM sessions s WHERE s.started_at IS NOT NULL`+filter, args...).
		Scan(&earliest); err != nil {
		return time.Time{}, fmt.Errorf("trend window start: %w", err)
	}
	if earliest == nil {
		return now.AddDate(0, 0, -7), nil
	}
	return *earliest, nil
}

// maxFleetMixModels keeps the busiest models as their own bands and folds the rest into an
// "other" catch-all, so the stack reads as a handful of tracked models plus a tail rather
// than a rainbow of one-session models.
const maxFleetMixModels = 6

// fleetMixFrom computes each model's token share per bucket. It sums total tokens (input +
// output + cache read + cache write) per (bucket, model) over the session_usage_daily
// rollup of scoped sessions, bucketing on the rollup's UTC day (when the usage happened,
// day-grained) with the whole-day window, then normalizes each bucket to percent and keeps
// the busiest models with the tail folded into "other". A NULL day is undated usage, off
// the time axis here as everywhere.
func (s *Store) fleetMixFrom(ctx context.Context, q querier, f AnalyticsFilter, g trendGrid) (FleetMix, error) {
	filter, args := f.clauseForRollupDay("sud.day")
	rows, err := q.Query(ctx,
		`SELECT `+g.sqlBucketDay("sud.day")+` AS b,
		        sud.model,
		        coalesce(sum(sud.input_tokens + sud.output_tokens + sud.cache_read_tokens + sud.cache_write_tokens), 0)
		   FROM session_usage_daily sud
		   JOIN sessions s ON s.id = sud.session_id
		  WHERE sud.day IS NOT NULL`+filter+`
		  GROUP BY 1, 2`, args...)
	if err != nil {
		return FleetMix{}, fmt.Errorf("fleet mix: %w", err)
	}
	defer rows.Close()

	// tokens[model][bucket] and per-model / per-bucket totals for the normalization.
	tokens := map[string][]int64{}
	modelTotal := map[string]int64{}
	bucketTotal := make([]int64, g.n())
	for rows.Next() {
		var b time.Time
		var model string
		var toks int64
		if err := rows.Scan(&b, &model, &toks); err != nil {
			return FleetMix{}, fmt.Errorf("scan fleet mix: %w", err)
		}
		i := g.index(b)
		if i < 0 || toks <= 0 {
			continue
		}
		if model == "" {
			model = "unknown"
		}
		if tokens[model] == nil {
			tokens[model] = make([]int64, g.n())
		}
		tokens[model][i] += toks
		modelTotal[model] += toks
		bucketTotal[i] += toks
	}
	if err := rows.Err(); err != nil {
		return FleetMix{}, fmt.Errorf("iterate fleet mix: %w", err)
	}
	if len(modelTotal) == 0 {
		return FleetMix{NewestFirst: -1}, nil
	}

	// Rank models by total tokens; keep the top N as their own bands, fold the rest into
	// an "other" series so the stack stays legible.
	names := make([]string, 0, len(modelTotal))
	for m := range modelTotal {
		names = append(names, m)
	}
	sort.Slice(names, func(a, b int) bool {
		if modelTotal[names[a]] != modelTotal[names[b]] {
			return modelTotal[names[a]] > modelTotal[names[b]]
		}
		return names[a] < names[b]
	})
	other := make([]int64, g.n())
	kept := names
	if len(names) > maxFleetMixModels {
		kept = names[:maxFleetMixModels]
		for _, m := range names[maxFleetMixModels:] {
			for i, t := range tokens[m] {
				other[i] += t
			}
		}
	}

	out := FleetMix{NewestFirst: -1}
	// The arrival callout reads each model's first tokens EVER (same non-time scope,
	// no window bound), not its first in-window bucket: an incumbent with a quiet
	// first day is not an arrival, and the whole-history minimum is immune to the
	// top-N fold below (see the FleetMix field comment). A first day at or before the
	// grid's opening bucket is an incumbent (index 0 is indistinguishable from the
	// window seam), and one past the grid cannot happen for a model the windowed scan
	// saw. Ties go to the busiest model so the callout is deterministic.
	unbounded := f
	unbounded.Since, unbounded.Until = time.Time{}, time.Time{}
	ffilter, fargs := unbounded.clauseForRollupDay("sud.day")
	frows, err := q.Query(ctx,
		`SELECT sud.model, min(sud.day)
		   FROM session_usage_daily sud
		   JOIN sessions s ON s.id = sud.session_id
		  WHERE sud.day IS NOT NULL`+ffilter+`
		  GROUP BY sud.model`, fargs...)
	if err != nil {
		return FleetMix{}, fmt.Errorf("fleet mix arrivals: %w", err)
	}
	defer frows.Close()
	for frows.Next() {
		var model string
		var first time.Time
		if err := frows.Scan(&model, &first); err != nil {
			return FleetMix{}, fmt.Errorf("scan fleet mix arrival: %w", err)
		}
		if model == "" || model == "unknown" {
			continue
		}
		if _, inWindow := tokens[model]; !inWindow {
			continue
		}
		i := g.index(first)
		if i <= 0 {
			continue
		}
		// Later bucket wins; ties go to the busier model, then the lesser name, so the
		// callout never flaps between two runs over the same corpus (GROUP BY returns
		// rows in no particular order).
		switch {
		case i > out.NewestFirst,
			i == out.NewestFirst && modelTotal[model] > modelTotal[out.NewestModel],
			i == out.NewestFirst && modelTotal[model] == modelTotal[out.NewestModel] && model < out.NewestModel:
			out.NewestFirst = i
			out.NewestModel = model
		}
	}
	if err := frows.Err(); err != nil {
		return FleetMix{}, fmt.Errorf("iterate fleet mix arrivals: %w", err)
	}
	build := func(name string, toks []int64) ModelSeries {
		share := make([]float64, g.n())
		first := -1
		for i := range toks {
			if bucketTotal[i] > 0 {
				share[i] = float64(toks[i]) / float64(bucketTotal[i]) * 100
			}
			if toks[i] > 0 && first < 0 {
				first = i
			}
		}
		return ModelSeries{Model: name, Share: share, First: first}
	}
	for _, m := range kept {
		out.Models = append(out.Models, build(m, tokens[m]))
	}
	var otherTotal int64
	for _, t := range other {
		otherTotal += t
	}
	if otherTotal > 0 {
		out.Models = append(out.Models, build("other", other))
	}
	return out, nil
}

// signalTrendsFrom computes the per-bucket grade, outcome, hygiene, and context-reset
// series from session_signals, plus the window-wide peak-context histogram. Every gated
// join matches the distributions (NOT s.signals_stale), so a bucket's rates speak only
// for the sessions a current signal actually covers.
func (s *Store) signalTrendsFrom(ctx context.Context, q querier, f AnalyticsFilter, g trendGrid, ctx0 ContextHealthStats) (SignalTrends, error) {
	out := SignalTrends{
		GradeShare:          make([]map[string]float64, g.n()),
		GPA:                 make([]float64, g.n()),
		ArchetypeShare:      make([]map[string]float64, g.n()),
		CompletedRate:       make([]float64, g.n()),
		AbandonedRate:       make([]float64, g.n()),
		OutcomeTotal:        make([]int, g.n()),
		HygieneTerse:        make([]float64, g.n()),
		HygieneRepeated:     make([]float64, g.n()),
		HygieneNoCode:       make([]float64, g.n()),
		HygieneUnstructured: make([]float64, g.n()),
		ContextResets:       make([]int, g.n()),
	}
	for i := range out.GradeShare {
		out.GradeShare[i] = map[string]float64{}
		out.ArchetypeShare[i] = map[string]float64{}
	}

	// Grades and outcomes: one scan per bucket over the gated cohort. A missing grade
	// folds into "" (unscored); a missing outcome into 'unknown'.
	filter, args := f.clauseFor("s.started_at")
	rows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT %s AS b, coalesce(sig.grade, '') AS grade, coalesce(sig.outcome, 'unknown') AS outcome, count(*)
		   FROM sessions s
		   LEFT JOIN session_signals sig
		     ON sig.session_id = s.id AND `+signalsCurrent()+`
		  WHERE s.started_at IS NOT NULL%s
		  GROUP BY 1, 2, 3`, g.sqlBucket("s.started_at"), filter), args...)
	if err != nil {
		return SignalTrends{}, fmt.Errorf("grade/outcome trend: %w", err)
	}
	// gradeCounts[bucket][grade] and outcome tallies, accumulated then normalized.
	gradeCounts := make([]map[string]int, g.n())
	for i := range gradeCounts {
		gradeCounts[i] = map[string]int{}
	}
	completed := make([]int, g.n())
	abandoned := make([]int, g.n())
	for rows.Next() {
		var b time.Time
		var grade, outcome string
		var n int
		if err := rows.Scan(&b, &grade, &outcome, &n); err != nil {
			rows.Close()
			return SignalTrends{}, fmt.Errorf("scan grade/outcome trend: %w", err)
		}
		i := g.index(b)
		if i < 0 {
			continue
		}
		gradeCounts[i][grade] += n
		out.OutcomeTotal[i] += n
		switch outcome {
		case string(quality.OutcomeCompleted):
			completed[i] += n
		case string(quality.OutcomeAbandoned):
			abandoned[i] += n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return SignalTrends{}, fmt.Errorf("iterate grade/outcome trend: %w", err)
	}
	for i := range gradeCounts {
		total := out.OutcomeTotal[i]
		if total == 0 {
			continue
		}
		var graded int
		var points float64
		for _, gk := range quality.GradeOrder {
			c := gradeCounts[i][gk]
			out.GradeShare[i][gk] = float64(c) / float64(total) * 100
			if gk != "" {
				graded += c
				points += quality.GPAPoints(gk) * float64(c)
			}
		}
		if graded > 0 {
			out.GPA[i] = points / float64(graded)
		}
		out.CompletedRate[i] = float64(completed[i]) / float64(total) * 100
		out.AbandonedRate[i] = float64(abandoned[i]) / float64(total) * 100
	}
	// Expose the raw counts the rates were built from, so the outcome chart's bars partition the
	// same completed/abandoned/other split the store computed (both slices are already sized to
	// the grid and zero for an empty bucket).
	out.CompletedCount = completed
	out.AbandonedCount = abandoned

	// Archetype mix per bucket: the same banding CASE the distribution uses, grouped by bucket
	// and shape. It carries no session_signals join (a session's shape is a fact of its own
	// columns), so it counts every started session in the window, matching the archetype
	// distribution's denominator. archetypeCaseExpr's numeric thresholds are already interpolated
	// at init, so it enters this query as a literal fragment.
	filter, args = f.clauseFor("s.started_at")
	arows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT %s AS b, %s AS arch, count(*)
		   FROM sessions s
		  WHERE s.started_at IS NOT NULL%s
		  GROUP BY 1, 2`, g.sqlBucket("s.started_at"), archetypeCaseExpr, filter), args...)
	if err != nil {
		return SignalTrends{}, fmt.Errorf("archetype trend: %w", err)
	}
	archCounts := make([]map[string]int, g.n())
	for i := range archCounts {
		archCounts[i] = map[string]int{}
	}
	archTotal := make([]int, g.n())
	for arows.Next() {
		var b time.Time
		var arch string
		var n int
		if err := arows.Scan(&b, &arch, &n); err != nil {
			arows.Close()
			return SignalTrends{}, fmt.Errorf("scan archetype trend: %w", err)
		}
		if i := g.index(b); i >= 0 {
			archCounts[i][arch] += n
			archTotal[i] += n
		}
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return SignalTrends{}, fmt.Errorf("iterate archetype trend: %w", err)
	}
	for i := range archCounts {
		if archTotal[i] == 0 {
			continue
		}
		for _, ak := range archetypeOrder {
			out.ArchetypeShare[i][ak] = float64(archCounts[i][ak]) / float64(archTotal[i]) * 100
		}
	}

	// Hygiene: per-bucket sums over the gated cohort. Rates divide by the bucket's
	// prompt total (or session total for unstructured starts).
	filter, args = f.clauseFor("s.started_at")
	hrows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT %s AS b,
		        coalesce(sum(sig.prompt_count), 0),
		        coalesce(sum(sig.short_prompt_count), 0),
		        coalesce(sum(sig.duplicate_prompt_count), 0),
		        coalesce(sum(sig.no_code_context_count), 0),
		        count(*) FILTER (WHERE sig.session_id IS NOT NULL),
		        count(*) FILTER (WHERE sig.unstructured_start)
		   FROM sessions s
		   LEFT JOIN session_signals sig
		     ON sig.session_id = s.id AND `+signalsCurrent()+`
		  WHERE s.started_at IS NOT NULL%s
		  GROUP BY 1`, g.sqlBucket("s.started_at"), filter), args...)
	if err != nil {
		return SignalTrends{}, fmt.Errorf("hygiene trend: %w", err)
	}
	for hrows.Next() {
		var b time.Time
		var prompts, short, dup, nocode, sessions, unstructured int
		if err := hrows.Scan(&b, &prompts, &short, &dup, &nocode, &sessions, &unstructured); err != nil {
			hrows.Close()
			return SignalTrends{}, fmt.Errorf("scan hygiene trend: %w", err)
		}
		i := g.index(b)
		if i < 0 {
			continue
		}
		if prompts > 0 {
			out.HygieneTerse[i] = float64(short) / float64(prompts) * 100
			out.HygieneRepeated[i] = float64(dup) / float64(prompts) * 100
			out.HygieneNoCode[i] = float64(nocode) / float64(prompts) * 100
		}
		if sessions > 0 {
			out.HygieneUnstructured[i] = float64(unstructured) / float64(sessions) * 100
		}
	}
	hrows.Close()
	if err := hrows.Err(); err != nil {
		return SignalTrends{}, fmt.Errorf("iterate hygiene trend: %w", err)
	}

	// Context resets per bucket, over sessions carrying a measured peak.
	filter, args = f.clauseFor("s.started_at")
	crows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT %s AS b, coalesce(sum(sig.context_reset_count), 0)
		   FROM sessions s
		   JOIN session_signals sig
		     ON sig.session_id = s.id AND `+signalsCurrent()+`
		  WHERE s.started_at IS NOT NULL AND sig.peak_context_tokens IS NOT NULL%s
		  GROUP BY 1`, g.sqlBucket("s.started_at"), filter), args...)
	if err != nil {
		return SignalTrends{}, fmt.Errorf("context reset trend: %w", err)
	}
	for crows.Next() {
		var b time.Time
		var resets int
		if err := crows.Scan(&b, &resets); err != nil {
			crows.Close()
			return SignalTrends{}, fmt.Errorf("scan context reset trend: %w", err)
		}
		if i := g.index(b); i >= 0 {
			out.ContextResets[i] = resets
		}
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return SignalTrends{}, fmt.Errorf("iterate context reset trend: %w", err)
	}

	// Peak-context histogram over the whole window (not per bucket): a log-scale count of
	// how heavy sessions got, with the order-statistic markers from the context panel.
	hist, err := s.contextHistogramFrom(ctx, q, f)
	if err != nil {
		return SignalTrends{}, err
	}
	out.ContextHistogram = hist
	if ctx0.HasData() {
		out.ContextMarkers = []ContextMarker{
			{Tokens: ctx0.PeakTokensP50, Kind: "p50"},
			{Tokens: ctx0.PeakTokensP90, Kind: "p90"},
			{Tokens: ctx0.PeakTokensMax, Kind: "max"},
		}
	}
	return out, nil
}

// contextHistogramEdges are the log-scale bin edges (powers of two, 8k..1M) the peak-context
// histogram counts into, matching the concept's octave bins so a heavy-context tail reads at
// a glance.
var contextHistogramEdges = func() []int64 {
	var edges []int64
	for e := int64(8000); e <= 1024000; e *= 2 {
		edges = append(edges, e)
	}
	return edges
}()

// contextHistogramFrom counts scoped sessions into the log-scale peak-context bins. It reads
// the same gated peak the context panel does, so the histogram's total reconciles with the
// context cohort.
func (s *Store) contextHistogramFrom(ctx context.Context, q querier, f AnalyticsFilter) ([]ContextBucket, error) {
	filter, args := f.clauseFor("s.started_at")
	rows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT sig.peak_context_tokens
		   FROM sessions s
		   JOIN session_signals sig
		     ON sig.session_id = s.id AND `+signalsCurrent()+`
		  WHERE s.started_at IS NOT NULL AND sig.peak_context_tokens IS NOT NULL%s`,
		filter), args...)
	if err != nil {
		return nil, fmt.Errorf("context histogram: %w", err)
	}
	defer rows.Close()
	buckets := make([]ContextBucket, len(contextHistogramEdges)-1)
	for i := range buckets {
		buckets[i] = ContextBucket{Lo: contextHistogramEdges[i], Hi: contextHistogramEdges[i+1]}
	}
	for rows.Next() {
		var peak int64
		if err := rows.Scan(&peak); err != nil {
			return nil, fmt.Errorf("scan context histogram: %w", err)
		}
		// First bin whose upper edge clears the peak, folding a sub-8k peak into the first
		// bin and an over-1M peak into the last. Without the underflow fold a small measured
		// session would fall through every bin and the histogram total would undercount the
		// context cohort (ContextHealthStats.Sessions), which counts every non-null peak.
		for i := range buckets {
			if peak < buckets[i].Hi || i == len(buckets)-1 {
				buckets[i].Count++
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate context histogram: %w", err)
	}
	return buckets, nil
}

// economicsFrom computes the per-bucket spend split by session outcome and the cache
// savings, over the session_usage_daily rollup. Spend buckets on the rollup's UTC day
// (when the money was spent, day-grained) and is gated to the session's outcome so
// completed-vs-abandoned dollars reconcile with the outcome distribution; the outcome
// joins in from session_signals at read time per the rollup grain rule. Cache savings is
// priced at day-and-model granularity (so a bucket that straddles a rate change still
// prices each day correctly) then re-bucketed to the grid.
func (s *Store) economicsFrom(ctx context.Context, q querier, f AnalyticsFilter, g trendGrid) (Economics, error) {
	out := Economics{
		CostCompleted: make([]float64, g.n()),
		CostAbandoned: make([]float64, g.n()),
		CostOther:     make([]float64, g.n()),
		CacheSavings:  make([]float64, g.n()),
		CacheHitRate:  make([]float64, g.n()),
		CacheMeasured: make([]bool, g.n()),
	}

	filter, args := f.clauseForRollupDay("sud.day")
	rows, err := q.Query(ctx, fmt.Sprintf(
		`SELECT %s AS b,
		        coalesce(sum(sud.cost_usd) FILTER (WHERE sig.outcome = 'completed'), 0),
		        coalesce(sum(sud.cost_usd) FILTER (WHERE sig.outcome = 'abandoned'), 0),
		        coalesce(sum(sud.cost_usd), 0),
		        coalesce(sum(sud.cache_read_tokens), 0),
		        coalesce(sum(sud.input_tokens), 0),
		        coalesce(sum(sud.cache_write_tokens), 0),
		        coalesce(bool_or(sud.unpriced), false),
		        coalesce(bool_or(sud.unpriced) FILTER (WHERE sig.outcome = 'abandoned'), false)
		   FROM session_usage_daily sud
		   JOIN sessions s ON s.id = sud.session_id
		   LEFT JOIN session_signals sig
		     ON sig.session_id = s.id AND `+signalsCurrent()+`
		  WHERE sud.day IS NOT NULL%s
		  GROUP BY 1`, g.sqlBucketDay("sud.day"), filter), args...)
	if err != nil {
		return Economics{}, fmt.Errorf("cost of quality trend: %w", err)
	}
	for rows.Next() {
		var b time.Time
		var comp, aband, total float64
		var cacheRead, input, cacheWrite int64
		var incomplete, abandIncomplete bool
		if err := rows.Scan(&b, &comp, &aband, &total, &cacheRead, &input, &cacheWrite, &incomplete, &abandIncomplete); err != nil {
			rows.Close()
			return Economics{}, fmt.Errorf("scan cost of quality trend: %w", err)
		}
		// A window is incomplete if any bucket carried a token-bearing unpriced event, even one
		// the grid drops, so the flag folds before the index guard. The abandoned-subset flag
		// folds the same way, so the abandoned subfigure carries its own lower-bound marker.
		out.CostIncomplete = out.CostIncomplete || incomplete
		out.AbandonedIncomplete = out.AbandonedIncomplete || abandIncomplete
		i := g.index(b)
		if i < 0 {
			continue
		}
		// Completed and abandoned are the outcome projection's own buckets, so these dollars
		// read against the outcome distribution: abandoned is outcome='abandoned' only, not
		// every non-completed session (the same way the outcome split treats it). Other is every
		// dollar that is neither (errored, unknown, or a session with no current-version signal);
		// carrying it as its own band makes the three bands sum to total spend, so the stacked
		// chart reconciles with the '$ total spend' headline instead of hiding non-outcome
		// dollars in the gap between the bars and the total. Float summation can leave a sub-cent
		// negative residue, so clamp it.
		out.CostCompleted[i] = comp
		out.CostAbandoned[i] = aband
		if other := total - comp - aband; other > 0 {
			out.CostOther[i] = other
		}
		out.TotalSpend += total
		out.TotalAbandoned += aband
		// Cache hit rate is cache_read over all prompt-side tokens (input + cache_read +
		// cache_write), the same denominator the canonical cache tile uses, so the trend and
		// the tile never disagree. A bucket with prompt tokens but no cache reads is a real 0%,
		// so record whether the bucket was measured rather than reading a 0 rate as "no data".
		if denom := input + cacheRead + cacheWrite; denom > 0 {
			out.CacheHitRate[i] = float64(cacheRead) / float64(denom) * 100
			out.CacheMeasured[i] = true
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return Economics{}, fmt.Errorf("iterate cost of quality trend: %w", err)
	}

	if err := s.cacheSavingsTrend(ctx, q, f, g, &out); err != nil {
		return Economics{}, err
	}

	for i := range out.CacheSavings {
		out.TotalCacheSavings += out.CacheSavings[i]
	}
	if out.TotalSpend > 0 {
		out.AbandonedSharePct = out.TotalAbandoned / out.TotalSpend * 100
	}
	// The headline hit rate is the latest measured bucket's rate, including a genuine 0%. The old
	// scan stopped at the latest nonzero bucket, so an idle or cache-cold latest bucket made the
	// headline reuse a stale earlier rate the series no longer showed.
	for i := len(out.CacheMeasured) - 1; i >= 0; i-- {
		if out.CacheMeasured[i] {
			out.CacheHitRateLatest = out.CacheHitRate[i]
			break
		}
	}
	return out, nil
}

// cacheSavingsTrend prices what caching saved and folds it into the grid. The
// session_usage_daily rollup already sits at day-and-model granularity (exactly what
// pricing's date-effective windows need), so each (day, model) row group prices with
// pricing.CacheSavings at that day's rate, then the day's savings sum into its trend
// bucket, so a weekly bucket that spans a rate change still totals correctly.
func (s *Store) cacheSavingsTrend(ctx context.Context, q querier, f AnalyticsFilter, g trendGrid, out *Economics) error {
	filter, args := f.clauseForRollupDay("sud.day")
	rows, err := q.Query(ctx,
		`SELECT sud.day::timestamp AS d,
		        sud.model,
		        coalesce(sum(sud.cache_read_tokens), 0),
		        coalesce(sum(sud.cache_write_tokens), 0)
		   FROM session_usage_daily sud
		   JOIN sessions s ON s.id = sud.session_id
		  WHERE sud.day IS NOT NULL`+filter+`
		  GROUP BY 1, 2`, args...)
	if err != nil {
		return fmt.Errorf("cache savings trend: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var day time.Time
		var model string
		var cacheRead, cacheWrite int64
		if err := rows.Scan(&day, &model, &cacheRead, &cacheWrite); err != nil {
			return fmt.Errorf("scan cache savings trend: %w", err)
		}
		// Cached volume on a model the pricing table cannot price omits its saving and marks the
		// total partial, the same fold CacheStats does, so the insights savings figure carries the
		// same caveat as the overview cache tile. The check precedes the index guard so an event
		// the grid drops still flags the window.
		saved, ok := pricing.CacheSavings(model, day, cacheRead, cacheWrite)
		if !ok && (cacheRead > 0 || cacheWrite > 0) {
			out.CacheSavingsIncomplete = true
		}
		i := g.index(day)
		if i < 0 {
			continue
		}
		if ok {
			out.CacheSavings[i] += saved
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate cache savings trend: %w", err)
	}
	return nil
}
