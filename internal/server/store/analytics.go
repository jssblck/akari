package store

import (
	"context"
	"fmt"
	"strings"
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
// project or (with projectID 0) the whole instance, and optionally bounded to a
// trailing time window.
type Analytics struct {
	Series          []DayPoint
	Models          []Breakdown
	Agents          []Breakdown
	TotalCost       float64
	TotalIn         int64
	TotalOut        int64
	TotalCacheRead  int64
	TotalCacheWrite int64
	Sessions        int
}

// HasData reports whether there is anything worth charting, so the view can show
// an empty state instead of an axis with no line.
func (a Analytics) HasData() bool {
	return a.Sessions > 0 || a.TotalCost > 0 || len(a.Series) > 0
}

// TotalTokens is the combined token volume across all four classes (input,
// output, cache read, cache write). The overview's Tokens readout shows this one
// figure and keeps the per-class split behind its tooltip.
func (a Analytics) TotalTokens() int64 {
	return a.TotalIn + a.TotalOut + a.TotalCacheRead + a.TotalCacheWrite
}

// Analytics aggregates usage for the charts. projectID 0 means the whole
// instance; a non-zero id scopes every query to that project. A non-zero `since`
// bounds every rollup to events at or after that instant; the zero time means
// all of history. A non-empty userIDs scopes every rollup to sessions owned by
// those users; nil or empty means every user. It runs three rollups (daily
// series, by-model, by-agent) and derives the headline totals from them.
//
// With no time bound the by-agent split and its totals come from the
// authoritative session rollups, so unpriced or undated usage still counts. A
// time bound has to slice usage by event time, which only the usage_events have,
// so the bounded path derives every figure from those events (undated events,
// having no place on the time axis, fall out of the bounded view).
func (s *Store) Analytics(ctx context.Context, projectID int64, since time.Time, userIDs []int64) (Analytics, error) {
	var a Analytics

	series, err := s.analyticsSeries(ctx, projectID, since, userIDs)
	if err != nil {
		return a, err
	}
	a.Series = series

	models, err := s.analyticsByModel(ctx, projectID, since, userIDs)
	if err != nil {
		return a, err
	}
	a.Models = models

	agents, err := s.analyticsByAgent(ctx, projectID, since, userIDs)
	if err != nil {
		return a, err
	}
	a.Agents = agents

	// Cost and session counts come from the agent breakdown (each session maps to
	// one agent, so distinct counts sum cleanly); token totals come from the daily
	// series, which carries the per-class split the Tokens tooltip needs.
	for _, ag := range agents {
		a.TotalCost += ag.CostUSD
		a.Sessions += ag.Sessions
	}
	for _, p := range series {
		a.TotalIn += p.Input
		a.TotalOut += p.Output
		a.TotalCacheRead += p.CacheRead
		a.TotalCacheWrite += p.CacheWrite
	}
	return a, nil
}

// usageFilter builds the conjunctive WHERE additions for a usage_events query
// joined to sessions as `s`: the optional project scope, the optional set-of-users
// scope, and the optional trailing time bound. Placeholders are numbered so it can
// follow an existing WHERE clause that already opened the predicate.
func usageFilter(projectID int64, since time.Time, userIDs []int64) (string, []any) {
	var clauses string
	var args []any
	if projectID != 0 {
		args = append(args, projectID)
		clauses += fmt.Sprintf(" AND s.project_id = $%d", len(args))
	}
	if len(userIDs) > 0 {
		args = append(args, userIDs)
		clauses += fmt.Sprintf(" AND s.user_id = ANY($%d)", len(args))
	}
	if !since.IsZero() {
		args = append(args, since)
		clauses += fmt.Sprintf(" AND ue.occurred_at >= $%d", len(args))
	}
	return clauses, args
}

// sessionScope builds the optional WHERE predicates for a query that reads the
// sessions table aliased `s` directly (no usage_events join, so no prior WHERE to
// append to): the project scope and the set-of-users scope, either or both
// optional. It opens its own WHERE clause and returns an empty string when nothing
// is scoped.
func sessionScope(projectID int64, userIDs []int64) (string, []any) {
	var preds []string
	var args []any
	if projectID != 0 {
		args = append(args, projectID)
		preds = append(preds, fmt.Sprintf("s.project_id = $%d", len(args)))
	}
	if len(userIDs) > 0 {
		args = append(args, userIDs)
		preds = append(preds, fmt.Sprintf("s.user_id = ANY($%d)", len(args)))
	}
	if len(preds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(preds, " AND "), args
}

func (s *Store) analyticsSeries(ctx context.Context, projectID int64, since time.Time, userIDs []int64) ([]DayPoint, error) {
	filter, args := usageFilter(projectID, since, userIDs)
	rows, err := s.Pool.Query(ctx,
		`SELECT date_trunc('day', ue.occurred_at) AS day,
		        coalesce(sum(ue.cost_usd), 0),
		        coalesce(sum(ue.input_tokens), 0),
		        coalesce(sum(ue.output_tokens), 0),
		        coalesce(sum(ue.cache_read_tokens), 0),
		        coalesce(sum(ue.cache_write_tokens), 0)
		   FROM usage_events ue
		   JOIN sessions s ON s.id = ue.session_id
		  WHERE ue.occurred_at IS NOT NULL`+filter+`
		  GROUP BY day
		  ORDER BY day`, args...)
	if err != nil {
		return nil, fmt.Errorf("query analytics daily series: %w", err)
	}
	defer rows.Close()
	var out []DayPoint
	for rows.Next() {
		var p DayPoint
		if err := rows.Scan(&p.Day, &p.CostUSD, &p.Input, &p.Output, &p.CacheRead, &p.CacheWrite); err != nil {
			return nil, fmt.Errorf("scan analytics daily series: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analytics daily series: %w", err)
	}
	return out, nil
}

func (s *Store) analyticsByModel(ctx context.Context, projectID int64, since time.Time, userIDs []int64) ([]Breakdown, error) {
	filter, args := usageFilter(projectID, since, userIDs)
	rows, err := s.Pool.Query(ctx,
		`SELECT ue.model,
		        coalesce(sum(ue.cost_usd), 0),
		        coalesce(sum(ue.input_tokens + ue.output_tokens), 0),
		        count(DISTINCT ue.session_id)
		   FROM usage_events ue
		   JOIN sessions s ON s.id = ue.session_id
		  WHERE ue.model <> ''`+filter+`
		  GROUP BY ue.model
		  ORDER BY 2 DESC, 3 DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("query analytics by model: %w", err)
	}
	defer rows.Close()
	out, err := scanBreakdowns(rows)
	if err != nil {
		return nil, fmt.Errorf("scan analytics by model: %w", err)
	}
	return out, nil
}

func (s *Store) analyticsByAgent(ctx context.Context, projectID int64, since time.Time, userIDs []int64) ([]Breakdown, error) {
	// Bounded to a window, the split has to slice usage by event time, which lives
	// on usage_events; the unbounded view reads the authoritative session rollups.
	if !since.IsZero() {
		filter, args := usageFilter(projectID, since, userIDs)
		rows, err := s.Pool.Query(ctx,
			`SELECT s.agent,
			        coalesce(sum(ue.cost_usd), 0),
			        coalesce(sum(ue.input_tokens + ue.output_tokens), 0),
			        count(DISTINCT ue.session_id)
			   FROM usage_events ue
			   JOIN sessions s ON s.id = ue.session_id
			  WHERE ue.occurred_at IS NOT NULL`+filter+`
			  GROUP BY s.agent
			  ORDER BY 2 DESC, 4 DESC`, args...)
		if err != nil {
			return nil, fmt.Errorf("query windowed analytics by agent: %w", err)
		}
		defer rows.Close()
		out, err := scanBreakdowns(rows)
		if err != nil {
			return nil, fmt.Errorf("scan windowed analytics by agent: %w", err)
		}
		return out, nil
	}

	where, args := sessionScope(projectID, userIDs)
	rows, err := s.Pool.Query(ctx,
		`SELECT s.agent,
		        coalesce(sum(s.total_cost_usd), 0),
		        coalesce(sum(s.total_input_tokens + s.total_output_tokens), 0),
		        count(*)
		   FROM sessions s`+where+`
		  GROUP BY s.agent
		  ORDER BY 2 DESC, 4 DESC`, args...)
	if err != nil {
		return nil, fmt.Errorf("query analytics by agent: %w", err)
	}
	defer rows.Close()
	out, err := scanBreakdowns(rows)
	if err != nil {
		return nil, fmt.Errorf("scan analytics by agent: %w", err)
	}
	return out, nil
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
