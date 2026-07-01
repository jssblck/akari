package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jssblck/akari/internal/pricing"
)

// CacheStats summarizes prompt-cache effectiveness over a scope: the prompt token
// volume split into uncached input, cached reads, and cache writes (creation), the
// output volume alongside, and the USD caching saved versus paying the uncached input
// rate for the same prompt tokens. It backs the Cache readout on the overview, project,
// and session views, the cache counterpart to the cost and token figures beside it.
type CacheStats struct {
	Input      int64 // uncached prompt tokens, billed at the input rate
	Output     int64
	CacheRead  int64 // prompt tokens served from cache (the discounted read)
	CacheWrite int64 // prompt tokens written to cache (creation)
	SavingsUSD float64
	// SavingsIncomplete is true when some cached read or write volume rode an unpriced
	// model, so that model's saving is omitted and SavingsUSD is partial. Unlike cost,
	// this is NOT a clean lower bound: an omitted model's saving can be negative (a
	// Claude cache write is priced above input, a cost paid up front), so the true
	// figure could be lower OR higher than what is shown. The Cache readout flags it
	// "partial" rather than the cost figures' "$X+" lower-bound marker.
	SavingsIncomplete bool
}

// PromptTokens is the total prompt-side token volume: uncached input plus cached reads
// plus cache writes. Output is excluded; caching is a prompt-side economy, so the hit
// rate measures the prompt, not the completion.
func (c CacheStats) PromptTokens() int64 { return c.Input + c.CacheRead + c.CacheWrite }

// HitRate is the share of prompt tokens served from cache, 0..1. Cache writes count as
// misses: a token written to cache was read fresh on that turn, and only a later read
// of it is a hit. Zero when there are no prompt tokens, so a no-usage scope reads 0%
// rather than dividing by zero.
func (c CacheStats) HitRate() float64 {
	p := c.PromptTokens()
	if p == 0 {
		return 0
	}
	return float64(c.CacheRead) / float64(p)
}

// HasData reports whether any prompt tokens were seen, so a view can show an empty
// state instead of a 0% hit rate and a $0 saving on a scope with no usage.
func (c CacheStats) HasData() bool { return c.PromptTokens() > 0 }

// cacheModelRow is one model's cache-relevant token sums over a scope, the unit
// CacheStats folds. The saving is priced per model (the input-versus-cache rate gap
// differs across families, and is even negative on cache writes for Claude), so the
// fold has to see each model, not just the grand totals.
type cacheModelRow struct {
	Model      string
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// cacheStatsFrom folds per-model token sums into a CacheStats, pricing each model's
// saving through the rate table and flagging the result incomplete when cached volume
// rode an unpriced model (so the saving omits it). It is the shared core of the scoped
// (CacheStats) and per-session (SessionCacheStats) paths, so both compute the figure
// identically; only the rows they feed it differ.
func cacheStatsFrom(rows []cacheModelRow) CacheStats {
	var c CacheStats
	for _, r := range rows {
		c.Input += r.Input
		c.Output += r.Output
		c.CacheRead += r.CacheRead
		c.CacheWrite += r.CacheWrite
		if saving, ok := pricing.CacheSavings(r.Model, r.CacheRead, r.CacheWrite); ok {
			c.SavingsUSD += saving
		} else if r.CacheRead > 0 || r.CacheWrite > 0 {
			// Cached volume on a model the pricing table does not know: the saving omits
			// it and the flag says the figure is partial. The omitted term can be either
			// sign, so this is not a lower bound the way cost_incomplete is for cost.
			c.SavingsIncomplete = true
		}
	}
	return c
}

// CacheStats aggregates prompt-cache effectiveness over the analytics scope. It shares
// the analytics base exactly: the scoped dated usage_events (occurred_at IS NOT NULL),
// grouped by model, so the cache figures reconcile with the usage panel they sit beside
// rather than counting undated usage the panel drops off its time axis. Savings is
// folded per model in Go because pricing is compiled into the binary, not in the
// database, so the rate gap that defines a saving is not a column to sum.
//
// It reads on its own pooled connection. The snapshot path (AnalyticsSnapshot) instead
// threads its transaction through cacheStats, so the Cache tile and the token totals come
// from one MVCC snapshot and one connection rather than two.
func (s *Store) CacheStats(ctx context.Context, f AnalyticsFilter) (CacheStats, error) {
	return s.cacheStats(ctx, s.Pool, f)
}

// cacheStats runs the scoped cache aggregate on the given querier. Threading the querier is
// what lets analyticsFrom read the Cache tile inside its snapshot transaction: reconciling
// with the headline token classes requires the same snapshot (a second pooled connection
// could straddle a concurrent ingest or reparse), and staying on the one connection avoids
// a snapshot render holding two pool connections at once.
func (s *Store) cacheStats(ctx context.Context, q querier, f AnalyticsFilter) (CacheStats, error) {
	filter, args := f.clause()
	rows, err := q.Query(ctx,
		`SELECT ue.model,
		        coalesce(sum(ue.input_tokens), 0),
		        coalesce(sum(ue.output_tokens), 0),
		        coalesce(sum(ue.cache_read_tokens), 0),
		        coalesce(sum(ue.cache_write_tokens), 0)
		   FROM usage_events ue
		   JOIN sessions s ON s.id = ue.session_id
		  WHERE ue.occurred_at IS NOT NULL`+filter+`
		  GROUP BY ue.model`, args...)
	if err != nil {
		return CacheStats{}, fmt.Errorf("query cache stats: %w", err)
	}
	defer rows.Close()
	mrows, err := scanCacheModelRows(rows)
	if err != nil {
		return CacheStats{}, err
	}
	return cacheStatsFrom(mrows), nil
}

// SessionCacheStats recomputes one session's cache effectiveness by scanning its usage
// rows and pricing per model. Unlike the scoped CacheStats it counts ALL the session's
// usage, dated or not, so its prompt totals match the session's token rollups
// (sessions.total_*); the scoped path keeps the dated guard to match the time-bounded
// panel instead. That mirrors the one documented rollup-versus-analytics gap the cost and
// token figures already carry: a per-session figure counts every usage row, an analytics
// figure counts only the dated rows it can plot.
//
// The session header no longer calls this on the hot path: it reads the same figure off
// the total_cache_savings_usd rollup (folded per row at parse time), which is O(1) rather
// than a per-refresh scan. This full recompute stays as the independent oracle the
// reconciliation test prices the rollup against, so a drift between the parse-time fold and
// a from-scratch per-model recompute fails a test rather than shipping a wrong tile.
func (s *Store) SessionCacheStats(ctx context.Context, sessionID int64) (CacheStats, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT model,
		        coalesce(sum(input_tokens), 0),
		        coalesce(sum(output_tokens), 0),
		        coalesce(sum(cache_read_tokens), 0),
		        coalesce(sum(cache_write_tokens), 0)
		   FROM usage_events
		  WHERE session_id = $1
		  GROUP BY model`, sessionID)
	if err != nil {
		return CacheStats{}, fmt.Errorf("query session cache stats for session %d: %w", sessionID, err)
	}
	defer rows.Close()
	mrows, err := scanCacheModelRows(rows)
	if err != nil {
		return CacheStats{}, err
	}
	return cacheStatsFrom(mrows), nil
}

func scanCacheModelRows(rows pgx.Rows) ([]cacheModelRow, error) {
	var out []cacheModelRow
	for rows.Next() {
		var r cacheModelRow
		if err := rows.Scan(&r.Model, &r.Input, &r.Output, &r.CacheRead, &r.CacheWrite); err != nil {
			return nil, fmt.Errorf("scan cache model row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cache model rows: %w", err)
	}
	return out, nil
}
