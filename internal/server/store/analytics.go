package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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
// cost, its token volume broken out by class, and how many sessions it touched.
// The per-class split lets a slice both size its bar on the full token volume and
// reproduce the same hover card every other token figure carries.
type Breakdown struct {
	Label      string
	CostUSD    float64
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	// Reasoning is the slice's reasoning-token volume, the class some agents (codex, pi)
	// report for the model's hidden deliberation. It is tracked and shown on its own but
	// deliberately excluded from Tokens(): reasoning is neither a prompt nor a cache class,
	// so folding it into the bar-sizing total would double-count against the billed classes
	// and unsettle the headline-equals-sum reconciliation. It surfaces as its own figure.
	Reasoning int64
	Sessions  int
	// CostIncomplete is true when this slice folded in a usage event that carried
	// real token volume but no price (an unpriced model), so the slice's cost is a
	// lower bound. It lets a by-model or by-agent row show the same "$X+" marker the
	// per-session figures use rather than an exact cost that understates the slice.
	CostIncomplete bool
}

// Tokens is the all-class token volume (input, output, cache read, cache write)
// for the slice. The breakdown bars size and label on this, so a model's share
// reflects everything it was billed for. Sizing on uncached in/out alone (the old
// behavior) made cache-heavy models like Claude read as mispriced: a bar a third
// the width of its cost, because the cache tokens that drove the cost were absent
// from the figure beside it.
func (b Breakdown) Tokens() int64 {
	return b.Input + b.Output + b.CacheRead + b.CacheWrite
}

// Analytics is everything the inline charts render from, scoped by an
// AnalyticsFilter (a project or the whole instance, a trailing window, and an
// optional user/agent/machine narrowing).
type Analytics struct {
	Series          []DayPoint
	Models          []Breakdown
	Agents          []Breakdown
	TotalCost       float64
	TotalIn         int64
	TotalOut        int64
	TotalCacheRead  int64
	TotalCacheWrite int64
	// TotalReasoning is the window's reasoning-token volume, summed from the by-agent split
	// like the other totals. It sits beside TotalTokens rather than inside it (see
	// Breakdown.Reasoning for why), so the Tokens tile shows it as a distinct class without
	// disturbing the headline-equals-sum-of-series reconciliation the four billed classes hold.
	TotalReasoning int64
	Sessions       int
	// CostIncomplete is true when any usage event in the window carried token
	// volume but no price, so TotalCost is a lower bound. The headline Cost tile
	// shows the "$X+" marker when set, matching how a single session flags an
	// incomplete cost. It is the OR of the by-agent slices, the same rows the
	// headline totals are summed from.
	CostIncomplete bool
	// Cache is the prompt-cache effectiveness over the same scope: hit rate, the
	// dollars caching saved, and the prompt-token split. It reads from the same dated
	// usage_events base as the totals (see CacheStats), so the Cache tile reconciles
	// with the Tokens tile beside it rather than counting usage the panel drops.
	Cache CacheStats
}

// AnalyticsFilter scopes an Analytics query. The zero value is the whole instance,
// all of history, every user, every agent and machine. ProjectID 0 means all
// projects; a zero Since means all of history; an empty UserIDs/Agent/Machine means
// no scoping on that dimension. The project page sets Agent/User/Machine so its
// usage panel reflects the same filter as its session table, which keeps the panel
// headline and the rows beneath it reconciled under a filter rather than letting
// the headline stay instance-wide while the rows narrow.
type AnalyticsFilter struct {
	ProjectID int64
	Since     time.Time
	// Until is an exclusive upper bound on occurred_at; the zero value means no upper
	// bound (every figure through the latest event). The OG card sets it to the end
	// of the current day so its headline and caption cover exactly the days its
	// heatmap draws (the grid stops at today), rather than folding a future-dated
	// event into the total that no visible cell accounts for. The live pages leave it
	// zero.
	Until   time.Time
	UserIDs []int64
	// Username scopes by a single account name, the form the project page's filter
	// carries. It is independent of UserIDs (the overview's multi-select by id): an
	// unknown name resolves to no session, the same empty result the session list's
	// u.username = $ predicate gives, so the panel and the table stay in lockstep
	// even for a stale or mistyped user rather than the panel falling back to every
	// user while the table shows nothing.
	Username string
	Agent    string
	Machine  string
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

// Analytics aggregates usage for the charts, scoped by f (see AnalyticsFilter):
// project or whole instance, a trailing window, and an optional user/agent/machine
// narrowing applied uniformly to every base.
//
// Every figure derives from one base set, the scoped dated usage_events, so the
// headline totals, the daily series, the by-model split, and the by-agent split
// all reconcile by construction: sum the per-day cells, or the by-model tokens,
// or the by-agent tokens, and you get the headline tokens, every time. This is
// deliberate. The figures used to come from three different sources (tokens from
// the daily series, cost from the session rollups, the by-model split from a
// separate usage_events query that dropped unnamed models), so the headline and
// the rows beneath it could disagree by an order of magnitude. They are the same
// base now, grouped three ways (by day, by model, by agent), with the headline
// summed from one of them.
//
// Every base shares the one filter the time axis forces: occurred_at IS NOT NULL.
// An undated usage event has no day to plot, so counting it in the headline but
// not in the daily cells would make the total exceed the sum of the chart, the
// exact drift this view exists to avoid. So the overview counts dated usage
// only, uniformly. In practice that excludes nothing: Claude, Codex, and pi all
// stamp the turn a usage line belongs to, so a NULL occurred_at is a malformed
// transcript to fix at ingest, not usage to scatter across the dashboard.
//
// The headline totals are summed from the by-agent split rather than queried
// again: a session has exactly one agent, so the per-agent rows partition the
// usage cleanly and their sum is the grand total with no double counting.
func (s *Store) Analytics(ctx context.Context, f AnalyticsFilter) (Analytics, error) {
	return s.analyticsFrom(ctx, s.Pool, f)
}

// querier is the subset of *pgxpool.Pool and pgx.Tx that the analytics queries
// use, so the same query builders serve both a plain pool read (Analytics) and a
// transaction-scoped read (AnalyticsSnapshot).
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// analyticsFrom assembles the Analytics from one querier, so a pooled read and a
// single-transaction snapshot share the same three grouped queries and the same
// headline arithmetic.
func (s *Store) analyticsFrom(ctx context.Context, q querier, f AnalyticsFilter) (Analytics, error) {
	var a Analytics

	series, err := s.analyticsSeries(ctx, q, f)
	if err != nil {
		return a, err
	}
	a.Series = series

	models, err := s.analyticsByModel(ctx, q, f)
	if err != nil {
		return a, err
	}
	a.Models = models

	agents, err := s.analyticsByAgent(ctx, q, f)
	if err != nil {
		return a, err
	}
	a.Agents = agents

	// Sum the headline from the by-agent split so the total and the rows beneath it
	// are the same arithmetic. Agents partition sessions one-to-one, so the session
	// count sums without overlap; the by-model split shares the same grand total but
	// its per-model session counts can overlap (a session spanning two models is in
	// both rows), which is why the count comes from agents, not models.
	for _, ag := range agents {
		a.TotalCost += ag.CostUSD
		a.Sessions += ag.Sessions
		a.TotalIn += ag.Input
		a.TotalOut += ag.Output
		a.TotalCacheRead += ag.CacheRead
		a.TotalCacheWrite += ag.CacheWrite
		a.TotalReasoning += ag.Reasoning
		a.CostIncomplete = a.CostIncomplete || ag.CostIncomplete
	}

	// Cache effectiveness shares the same scoped dated-usage base, so its prompt
	// totals reconcile with the headline token classes above (its Input/CacheRead/
	// CacheWrite are the same sums, regrouped by model to price the saving).
	cache, err := s.CacheStats(ctx, f)
	if err != nil {
		return a, err
	}
	a.Cache = cache
	return a, nil
}

// AnalyticsSnapshot reads Analytics as a single consistent snapshot that is
// guaranteed not to straddle a reparse, for the OG card render. It runs the three
// grouped queries inside one REPEATABLE READ transaction, so they all read one MVCC
// snapshot rather than three independently-timed reads. The first statement in that
// transaction is the reparse-lock check, which both establishes the snapshot and
// reads the lock state at that instant: if no reparse holds the lock at the snapshot
// point, none is mid-flight, so every session in the snapshot is in a settled state
// (fully reparsed or untouched) rather than a half-rebuilt mix, and a reparse that
// starts later cannot alter the frozen snapshot. When a reparse does hold the lock
// at that instant, it returns ok=false and no analytics, so the caller skips the
// render rather than caching a mixed aggregate. Unlike taking the reparse advisory
// lock itself, this holds no lock, so it never makes the fleet read as "reparsing".
func (s *Store) AnalyticsSnapshot(ctx context.Context, f AnalyticsFilter) (a Analytics, ok bool, err error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return a, false, fmt.Errorf("begin analytics snapshot: %w", err)
	}
	defer tx.Rollback(ctx)

	var held bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM pg_locks
		   WHERE locktype = 'advisory'
		     AND classid = hashtext(current_database())::oid
		     AND objid = $1::oid
		     AND objsubid = 2
		     AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		 )`, reparseLockKey).Scan(&held); err != nil {
		return a, false, fmt.Errorf("check reparse lock in snapshot: %w", err)
	}
	if held {
		return a, false, nil
	}

	a, err = s.analyticsFrom(ctx, tx, f)
	if err != nil {
		return Analytics{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Analytics{}, false, fmt.Errorf("commit analytics snapshot: %w", err)
	}
	return a, true, nil
}

// clause builds the conjunctive WHERE additions for a usage_events query joined to
// sessions as `s`: the optional project, user-set, agent, and machine scopes, plus
// the optional trailing time bound on ue.occurred_at. Agent and machine read the
// verbatim session columns the session list filters on, so the panel and the table
// narrow by the same values. Placeholders are numbered so it can follow an existing
// WHERE clause that already opened the predicate.
func (f AnalyticsFilter) clause() (string, []any) {
	return f.clauseFor("ue.occurred_at")
}

// clauseFor is clause with the trailing-window bound applied to an arbitrary
// timestamp expression rather than ue.occurred_at, so a query over a session-derived
// table (session_signals, or sessions alone, which carry no usage_events to date) can
// reuse the identical project / user / agent / machine scoping with its own time
// column. Both trailing-window bounds (Since and Until) apply to timeExpr; every
// other predicate is the same, so the Insights distributions narrow by the same
// filter the usage panel does. A session whose time column is NULL falls outside
// either bound (the comparison is NULL, so not true), which is the right call: an
// undated session has no place on a windowed view, the same reasoning the usage
// base applies to undated usage.
func (f AnalyticsFilter) clauseFor(timeExpr string) (string, []any) {
	var clauses string
	var args []any
	if f.ProjectID != 0 {
		args = append(args, f.ProjectID)
		clauses += fmt.Sprintf(" AND s.project_id = $%d", len(args))
	}
	if len(f.UserIDs) > 0 {
		args = append(args, f.UserIDs)
		clauses += fmt.Sprintf(" AND s.user_id = ANY($%d)", len(args))
	}
	if f.Username != "" {
		// Scope by name through a scalar subquery rather than a pre-resolved id, so
		// an unknown name yields a NULL the equality never matches (an empty result),
		// exactly as the session list's u.username = $ filter does. No separate
		// lookup means no error path to swallow into a silent all-user fallback.
		args = append(args, f.Username)
		clauses += fmt.Sprintf(" AND s.user_id = (SELECT id FROM users WHERE username = $%d)", len(args))
	}
	if f.Agent != "" {
		args = append(args, f.Agent)
		clauses += fmt.Sprintf(" AND s.agent = $%d", len(args))
	}
	if f.Machine != "" {
		args = append(args, f.Machine)
		clauses += fmt.Sprintf(" AND s.machine = $%d", len(args))
	}
	if !f.Since.IsZero() {
		args = append(args, f.Since)
		clauses += fmt.Sprintf(" AND %s >= $%d", timeExpr, len(args))
	}
	if !f.Until.IsZero() {
		args = append(args, f.Until)
		clauses += fmt.Sprintf(" AND %s < $%d", timeExpr, len(args))
	}
	return clauses, args
}

func (s *Store) analyticsSeries(ctx context.Context, q querier, f AnalyticsFilter) ([]DayPoint, error) {
	filter, args := f.clause()
	rows, err := q.Query(ctx,
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

// costIncompleteExpr is the per-slice incompleteness aggregate shared by the
// by-model and by-agent splits: true when the slice folded in a usage event that
// carried real token volume but no price, so its summed cost is a lower bound. The
// token sum mirrors projection.go exactly (input + output + cache read + cache
// write + reasoning), so a reasoning-only unpriced row flags the breakdown the same
// way it already flags the session rollup; kept here as one expression so both
// splits agree.
const costIncompleteExpr = `bool_or(ue.cost_usd IS NULL AND (ue.input_tokens + ue.output_tokens + ue.cache_read_tokens + ue.cache_write_tokens + ue.reasoning_tokens) > 0)`

func (s *Store) analyticsByModel(ctx context.Context, q querier, f AnalyticsFilter) ([]Breakdown, error) {
	filter, args := f.clause()
	// occurred_at IS NOT NULL matches the series and the by-agent split, so the
	// three reconcile; see Analytics. No model <> '' filter though: usage that
	// carries no model id still has to be in the split, or the by-model rows would
	// sum to less than the headline. An unnamed model groups into its own row and
	// FoldUnknownModels collapses it (with every other unpriced model) into
	// "Other", so it counts without leaking a blank row.
	rows, err := q.Query(ctx,
		`SELECT ue.model,
		        coalesce(sum(ue.cost_usd), 0),
		        coalesce(sum(ue.input_tokens), 0),
		        coalesce(sum(ue.output_tokens), 0),
		        coalesce(sum(ue.cache_read_tokens), 0),
		        coalesce(sum(ue.cache_write_tokens), 0),
		        coalesce(sum(ue.reasoning_tokens), 0),
		        count(DISTINCT ue.session_id),
		        coalesce(`+costIncompleteExpr+`, false)
		   FROM usage_events ue
		   JOIN sessions s ON s.id = ue.session_id
		  WHERE ue.occurred_at IS NOT NULL`+filter+`
		  GROUP BY ue.model
		  ORDER BY 2 DESC, coalesce(sum(ue.input_tokens + ue.output_tokens + ue.cache_read_tokens + ue.cache_write_tokens), 0) DESC`, args...)
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

func (s *Store) analyticsByAgent(ctx context.Context, q querier, f AnalyticsFilter) ([]Breakdown, error) {
	// One path, the same dated usage_events base as the by-model split and the
	// series, so all of them reconcile with the headline (see Analytics for why the
	// occurred_at IS NOT NULL guard is uniform). The headline is summed from these
	// rows, so this query defines the totals; the rollups equal this sum by
	// construction (sessions.total_* == sum over a session's usage_events), so
	// reading the ledger here loses nothing the old session-rollup path carried.
	filter, args := f.clause()
	rows, err := q.Query(ctx,
		`SELECT s.agent,
		        coalesce(sum(ue.cost_usd), 0),
		        coalesce(sum(ue.input_tokens), 0),
		        coalesce(sum(ue.output_tokens), 0),
		        coalesce(sum(ue.cache_read_tokens), 0),
		        coalesce(sum(ue.cache_write_tokens), 0),
		        coalesce(sum(ue.reasoning_tokens), 0),
		        count(DISTINCT ue.session_id),
		        coalesce(`+costIncompleteExpr+`, false)
		   FROM usage_events ue
		   JOIN sessions s ON s.id = ue.session_id
		  WHERE ue.occurred_at IS NOT NULL`+filter+`
		  GROUP BY s.agent
		  ORDER BY 2 DESC, count(DISTINCT ue.session_id) DESC`, args...)
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
		if err := rows.Scan(&b.Label, &b.CostUSD, &b.Input, &b.Output, &b.CacheRead, &b.CacheWrite, &b.Reasoning, &b.Sessions, &b.CostIncomplete); err != nil {
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
