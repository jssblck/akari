package store

import (
	"context"
	"time"
)

// DayPoint is one day's aggregated usage, the unit of the analytics time series.
// Days with no priced usage events are absent rather than zero-filled; the chart
// layer decides how to render gaps.
type DayPoint struct {
	Day        time.Time
	CostUSD    float64
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// Breakdown is one slice of a by-model or by-agent split: a label with its rolled
// cost, its total non-cache tokens, and how many sessions it touched.
type Breakdown struct {
	Label    string
	CostUSD  float64
	Tokens   int64
	Sessions int
}

// Analytics is everything the inline charts render from, scoped either to one
// project or (with projectID 0) the whole instance.
type Analytics struct {
	Series    []DayPoint
	Models    []Breakdown
	Agents    []Breakdown
	TotalCost float64
	TotalIn   int64
	TotalOut  int64
	Sessions  int
}

// HasData reports whether there is anything worth charting, so the view can show
// an empty state instead of an axis with no line.
func (a Analytics) HasData() bool {
	return a.Sessions > 0 || a.TotalCost > 0 || len(a.Series) > 0
}

// Analytics aggregates usage for the charts. projectID 0 means the whole
// instance; a non-zero id scopes every query to that project. It runs three
// rollups (daily series, by-model, by-agent) and derives the headline totals
// from the authoritative session rollups so unpriced or undated usage still
// counts toward the totals even when it cannot land on the time axis.
func (s *Store) Analytics(ctx context.Context, projectID int64) (Analytics, error) {
	var a Analytics

	series, err := s.analyticsSeries(ctx, projectID)
	if err != nil {
		return a, err
	}
	a.Series = series

	models, err := s.analyticsByModel(ctx, projectID)
	if err != nil {
		return a, err
	}
	a.Models = models

	agents, err := s.analyticsByAgent(ctx, projectID)
	if err != nil {
		return a, err
	}
	a.Agents = agents

	// Headline totals come from the session rollups (authoritative), summed across
	// the agent breakdown which is itself derived from those rollups.
	for _, ag := range agents {
		a.TotalCost += ag.CostUSD
		a.Sessions += ag.Sessions
	}
	for _, p := range series {
		a.TotalIn += p.Input
		a.TotalOut += p.Output
	}
	return a, nil
}

// scopeUsage adds the project filter to a usage_events query joined as `s`.
func scopeUsage(projectID int64) (string, []any) {
	if projectID == 0 {
		return "", nil
	}
	return " AND s.project_id = $1", []any{projectID}
}

func (s *Store) analyticsSeries(ctx context.Context, projectID int64) ([]DayPoint, error) {
	scope, args := scopeUsage(projectID)
	rows, err := s.Pool.Query(ctx,
		`SELECT date_trunc('day', ue.occurred_at) AS day,
		        coalesce(sum(ue.cost_usd), 0),
		        coalesce(sum(ue.input_tokens), 0),
		        coalesce(sum(ue.output_tokens), 0),
		        coalesce(sum(ue.cache_read_tokens), 0),
		        coalesce(sum(ue.cache_write_tokens), 0)
		   FROM usage_events ue
		   JOIN sessions s ON s.id = ue.session_id
		  WHERE ue.occurred_at IS NOT NULL`+scope+`
		  GROUP BY day
		  ORDER BY day`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DayPoint
	for rows.Next() {
		var p DayPoint
		if err := rows.Scan(&p.Day, &p.CostUSD, &p.Input, &p.Output, &p.CacheRead, &p.CacheWrite); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) analyticsByModel(ctx context.Context, projectID int64) ([]Breakdown, error) {
	scope, args := scopeUsage(projectID)
	rows, err := s.Pool.Query(ctx,
		`SELECT ue.model,
		        coalesce(sum(ue.cost_usd), 0),
		        coalesce(sum(ue.input_tokens + ue.output_tokens), 0),
		        count(DISTINCT ue.session_id)
		   FROM usage_events ue
		   JOIN sessions s ON s.id = ue.session_id
		  WHERE ue.model <> ''`+scope+`
		  GROUP BY ue.model
		  ORDER BY 2 DESC, 3 DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBreakdowns(rows)
}

func (s *Store) analyticsByAgent(ctx context.Context, projectID int64) ([]Breakdown, error) {
	where := ""
	var args []any
	if projectID != 0 {
		where = " WHERE s.project_id = $1"
		args = []any{projectID}
	}
	rows, err := s.Pool.Query(ctx,
		`SELECT s.agent,
		        coalesce(sum(s.total_cost_usd), 0),
		        coalesce(sum(s.total_input_tokens + s.total_output_tokens), 0),
		        count(*)
		   FROM sessions s`+where+`
		  GROUP BY s.agent
		  ORDER BY 2 DESC, 4 DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBreakdowns(rows)
}

func scanBreakdowns(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]Breakdown, error) {
	var out []Breakdown
	for rows.Next() {
		var b Breakdown
		if err := rows.Scan(&b.Label, &b.CostUSD, &b.Tokens, &b.Sessions); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ProjectSparklines returns, per project id, a fixed-length slice of daily cost
// over the last `days` days (index 0 oldest, last index today, UTC). Projects
// with no recent usage are simply absent from the map; the index view renders a
// flat line for them. One query buckets in Go so the index stays a single round
// trip regardless of project count.
func (s *Store) ProjectSparklines(ctx context.Context, days int) (map[int64][]float64, error) {
	if days <= 0 {
		days = 30
	}
	rows, err := s.Pool.Query(ctx,
		`SELECT s.project_id,
		        date_trunc('day', ue.occurred_at) AS day,
		        coalesce(sum(ue.cost_usd), 0)
		   FROM usage_events ue
		   JOIN sessions s ON s.id = ue.session_id
		  WHERE ue.occurred_at IS NOT NULL
		    AND ue.occurred_at >= now() - make_interval(days => $1)
		  GROUP BY s.project_id, day`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Day 0 is the oldest bucket; align to UTC midnight so date_trunc days map
	// cleanly onto array offsets.
	start := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -(days - 1))
	out := map[int64][]float64{}
	for rows.Next() {
		var pid int64
		var day time.Time
		var cost float64
		if err := rows.Scan(&pid, &day, &cost); err != nil {
			return nil, err
		}
		idx := int(day.UTC().Sub(start).Hours() / 24)
		if idx < 0 || idx >= days {
			continue
		}
		vals := out[pid]
		if vals == nil {
			vals = make([]float64, days)
			out[pid] = vals
		}
		vals[idx] += cost
	}
	return out, rows.Err()
}
