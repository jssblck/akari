package store

import (
	"context"
	"fmt"
	"time"

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
	// "partial" because the omitted saving can run in either direction.
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

// foldCacheRows folds a (model, UTC day, token-sums) result set into a CacheStats as the
// rows stream off the connection, pricing each bucket's saving at the rate in effect that
// day and flagging the result incomplete when cached volume rode an unpriced model (so the
// saving omits it). The token totals sum across the buckets, so the grand figures are
// unaffected by the day split; only the saving is priced per bucket, which is what lets a
// model with a dated rate change price each side at its own rate.
//
// It folds each row directly rather than materializing the whole result, so peak memory is
// the fixed accumulator regardless of how many buckets the query returns. That matters
// because grouping by day can yield one bucket per model per day of the window, a count
// driven by the corpus span (scoped) or session length (per-session), so buffering every
// bucket would grow with input the server actually ingests.
//
// The saving is priced per model because the input-versus-cache rate gap differs across
// families (and is even negative on cache writes for Claude), and per day because a
// model's rate can change on a date. The rate boundaries are UTC-midnight-aligned (pinned
// by TestDatedWindowsStartAtUTCMidnight in internal/pricing), so a whole UTC day sits
// inside one rate window: pricing the day's sum at that day's rate is exact and matches
// the per-row parse-time fold that reconciles against it. A NULL day (an undated row, which
// the per-session path counts) folds as the zero time, selecting the earliest window, the
// same choice parse-time pricing makes for a row with no OccurredAt.
//
// It is the shared core of the scoped (CacheStats) and per-session (SessionCacheStats)
// paths, so both compute the figure identically; only the query feeding it differs. The
// caller owns closing rows.
func foldCacheRows(rows pgx.Rows) (CacheStats, error) {
	var c CacheStats
	for rows.Next() {
		var (
			model                                string
			day                                  *time.Time
			input, output, cacheRead, cacheWrite int64
		)
		if err := rows.Scan(&model, &day, &input, &output, &cacheRead, &cacheWrite); err != nil {
			return CacheStats{}, fmt.Errorf("scan cache model row: %w", err)
		}
		c.Input += input
		c.Output += output
		c.CacheRead += cacheRead
		c.CacheWrite += cacheWrite
		at := time.Time{}
		if day != nil {
			at = *day
		}
		if saving, ok := pricing.CacheSavings(model, at, cacheRead, cacheWrite); ok {
			c.SavingsUSD += saving
		} else if cacheRead > 0 || cacheWrite > 0 {
			// Cached volume on a model the pricing table does not know: the saving omits
			// it and the flag says the figure is partial. The omitted term can be either
			// sign, so this is not a lower bound the way cost_incomplete is for cost.
			c.SavingsIncomplete = true
		}
	}
	if err := rows.Err(); err != nil {
		return CacheStats{}, fmt.Errorf("iterate cache model rows: %w", err)
	}
	return c, nil
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
	// Group by model AND the UTC day so a model with a dated rate change prices each
	// day's cached volume at that day's rate; the day is always non-NULL here (the
	// occurred_at IS NOT NULL guard below). The token sums fold back together in
	// foldCacheRows, so the extra grouping key affects only how the saving is priced.
	rows, err := q.Query(ctx,
		`SELECT ue.model,
		        date_trunc('day', ue.occurred_at AT TIME ZONE 'UTC'),
		        coalesce(sum(ue.input_tokens), 0),
		        coalesce(sum(ue.output_tokens), 0),
		        coalesce(sum(ue.cache_read_tokens), 0),
		        coalesce(sum(ue.cache_write_tokens), 0)
		   FROM usage_events ue
		   JOIN sessions s ON s.id = ue.session_id
		  WHERE ue.occurred_at IS NOT NULL`+filter+`
		  GROUP BY ue.model, date_trunc('day', ue.occurred_at AT TIME ZONE 'UTC')`, args...)
	if err != nil {
		return CacheStats{}, fmt.Errorf("query cache stats: %w", err)
	}
	defer rows.Close()
	return foldCacheRows(rows)
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
	return s.sessionCacheStatsFrom(ctx, s.Pool, sessionID)
}

// sessionCacheStatsFrom recomputes one session's cache effectiveness from the given querier, so
// the pooled per-session read (SessionCacheStats) and the backfill's locked transaction share
// one query and one pricing fold. See SessionCacheStats for the all-usage-versus-dated contract.
func (s *Store) sessionCacheStatsFrom(ctx context.Context, q querier, sessionID int64) (CacheStats, error) {
	// Group by model AND the UTC day so a dated rate change prices each day at its own
	// rate, matching the per-row parse-time fold this recompute reconciles against. An
	// undated row (occurred_at NULL) buckets to a NULL day, scanned as the zero time,
	// which selects the earliest window: the same choice the parse-time fold makes for a
	// row with no OccurredAt, so the two stay reconciled.
	rows, err := q.Query(ctx,
		`SELECT model,
		        date_trunc('day', occurred_at AT TIME ZONE 'UTC'),
		        coalesce(sum(input_tokens), 0),
		        coalesce(sum(output_tokens), 0),
		        coalesce(sum(cache_read_tokens), 0),
		        coalesce(sum(cache_write_tokens), 0)
		   FROM usage_events
		  WHERE session_id = $1
		  GROUP BY model, date_trunc('day', occurred_at AT TIME ZONE 'UTC')`, sessionID)
	if err != nil {
		return CacheStats{}, fmt.Errorf("query session cache stats for session %d: %w", sessionID, err)
	}
	defer rows.Close()
	return foldCacheRows(rows)
}
