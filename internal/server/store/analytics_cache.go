package store

import (
	"context"
	"errors"
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

// BackfillCacheSavings prices each not-yet-backfilled session's cached usage into the
// total_cache_savings_usd rollup. The rollup is normally folded at parse time (applyDelta), and
// the epoch reparse fills it for the corpus, but a session that fails to reparse (a malformed
// transcript the parser cannot rebuild) keeps its old usage_events while the reparse rolls back,
// so its rollup would stay at a suspect value forever. This pass closes that gap by pricing the
// existing ledger directly, independent of the parse: the saving is a pure function of
// usage_events, so it is correct even when the transcript is not. It runs at startup after
// migrations.
//
// A candidate is a cache-bearing session whose cache_savings_backfilled flag is false: the
// migration marks every pre-existing cache-bearing session that way and defaults sessions ingested
// afterward to true (their fold starts from a correct empty base, so they are authoritative from
// creation). The flag, not the stored number, is the "needs backfill" signal on purpose: a
// session seeded at 0 that took a live append folds only the new rows, leaving a partial nonzero
// total, so "total is zero" would wrongly pass it over. Pricing recomputes the full value from
// usage_events and sets the flag, so the session stops being a candidate and the pass converges to
// a no-op that is safe to run every startup. Each session is priced under a row lock (see
// backfillCacheSavingsForSession) so a write can never clobber the live parse fold. It keyset-pages
// by id so peak memory is one batch of ids, and returns how many sessions it corrected.
//
// It first runs the pricing reconcile (reconcileCacheSavingsPricingIfNeeded): a rate change re-prices
// every cache-bearing session, not just never-folded ones, and a failed-reparse session would keep
// its old-priced rollup flagged backfilled=true out of the candidate set. The reconcile clears that
// flag across the cache-bearing corpus once per pricing.Version bump, so the drain below re-prices
// them at the current rates. On a steady-state startup (marker current) the reconcile is one O(1)
// read and the drain finds no candidates.
func (s *Store) BackfillCacheSavings(ctx context.Context) (int, error) {
	if err := s.reconcileCacheSavingsPricingIfNeeded(ctx); err != nil {
		return 0, err
	}
	total := 0
	var afterID int64
	for {
		ids, err := s.cacheSavingsBackfillBatch(ctx, afterID, cacheSavingsBackfillBatch)
		if err != nil {
			return total, err
		}
		for _, id := range ids {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			wrote, err := s.backfillCacheSavingsForSession(ctx, id)
			if err != nil {
				return total, err
			}
			if wrote {
				total++
			}
		}
		if len(ids) < cacheSavingsBackfillBatch {
			return total, nil // a short batch means the candidate set is drained
		}
		afterID = ids[len(ids)-1]
	}
}

// reconcileCacheSavingsPricingIfNeeded re-prices the cache-savings rollup across the corpus once per
// pricing.Version change. A reprice bumps pricing.Version (paired with a parse.Epoch bump); the epoch
// reparse re-folds the rollup for every session it can rebuild, but a session whose reparse fails
// keeps its old-priced rollup with cache_savings_backfilled=true, so the drain skips it and its tile
// drifts from a live SessionCacheStats recompute at the new rates forever. This marks every
// cache-bearing session cache_savings_backfilled=false when the pricing marker is behind, so the
// drain in BackfillCacheSavings re-prices them all from usage_events at the current rates.
//
// The mark is committed on its own, independent of any reparse, which is the whole point: it survives
// a failed reparse's rollback. A session that never re-parses is still re-priced from its (old)
// usage_events at the new rates, exactly what SessionCacheStats computes live, so rollup and recompute
// agree again. The clear touches every cache-bearing session, including ones already false: an
// already-false row keeps the same value but is still locked, which is what serializes an old binary's
// per-session backfill against this marker advance (see the UPDATE below). The EXISTS probe skips
// sessions with no cache volume.
//
// It is gated on a parse_meta marker, mirroring reconcileStaleVersionsIfNeeded for quality.Version: a
// steady-state startup reads the singleton (one O(1) read), finds the marker current, and skips the
// full-corpus UPDATE. A pricing.Version bump ships in a new binary, so the first startup after the
// upgrade sees the marker behind, marks the cache-bearing corpus once, and advances the marker.
//
// The marker read, the corpus mark, and the marker advance all run in ONE transaction that holds the
// parse_meta row lock (SELECT ... FOR UPDATE), the same rolling-deploy fix as the signals reconcile.
// During a pricing rollout an old binary at pricing.Version N-1 and a new one at N share the
// database. The lock serializes their reconciles and forces the version recheck to happen while the
// lock is held, so whichever binary runs second re-reads the marker the winner wrote and, finding it
// at or past its own version, does nothing. A plain marker compare-and-set is not enough: without the
// lock the version check and the corpus-clearing UPDATE are separate steps, so an old N-1 binary that
// read a behind marker could clear cache_savings_backfilled across the corpus AFTER a new N binary had
// already cleared, advanced, and re-priced, then re-price those rows at the OLD rates on its own
// drain. The compare-and-set stops the marker regressing but not that late clear; the lock stops both.
// Running the corpus mark and the marker advance in the same transaction also makes the pair atomic:
// a crash rolls both back, so the next startup repeats the mark cleanly.
func (s *Store) reconcileCacheSavingsPricingIfNeeded(ctx context.Context) error {
	if err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var priced int
		if err := tx.QueryRow(ctx,
			`SELECT cache_savings_priced_version FROM parse_meta WHERE id = TRUE FOR UPDATE`).Scan(&priced); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // the migration seeds the singleton; a missing row means nothing to lock or price against
			}
			return fmt.Errorf("lock cache_savings_priced_version: %w", err)
		}
		if priced >= pricing.Version {
			return nil // already priced at this version or a newer one; never step the marker back
		}
		// Clear the authoritative flag on EVERY cache-bearing session, with no `s.cache_savings_backfilled`
		// skip for rows already false. As in reconcileStaleVersions, the write to an already-false row is a
		// no-op on the value but takes the row LOCK, which is what serializes an old binary's per-session
		// backfill against this marker advance during a rolling deploy. Skipping already-false candidates
		// would leave the same hole the signals reconcile closes: an old backfill that had selected such a
		// candidate could lock it, read the pre-advance pricing marker, write an old-rate total, and set
		// cache_savings_backfilled=true, all while this reconcile (having skipped that row) advances the
		// marker and commits. The new drain then skips the now-true row and the session keeps an
		// authoritative rollup priced at the superseded rates. Locking every cache-bearing row closes it:
		// the old backfill either locks first (then this UPDATE waits and re-clears the flag after it
		// commits, so the new drain re-prices) or this UPDATE locks first (then the old backfill's
		// SELECT ... FOR UPDATE waits, reads the advanced marker, and bows out).
		if _, err := tx.Exec(ctx,
			`UPDATE sessions s SET cache_savings_backfilled = false
			  WHERE EXISTS (SELECT 1 FROM usage_events u
			                 WHERE u.session_id = s.id
			                   AND (u.cache_read_tokens > 0 OR u.cache_write_tokens > 0))`); err != nil {
			return fmt.Errorf("mark cache-bearing sessions for reprice: %w", err)
		}
		// The recheck above ran under the row lock, so the marker is provably still behind and no other
		// binary can move it while this transaction holds the lock: a plain advance is safe here.
		if _, err := tx.Exec(ctx,
			`UPDATE parse_meta SET cache_savings_priced_version = $1, updated_at = now() WHERE id = TRUE`,
			pricing.Version); err != nil {
			return fmt.Errorf("mark cache savings priced at version %d: %w", pricing.Version, err)
		}
		return nil
	}); err != nil {
		// Wrap the transaction result too, so a begin or commit failure (which never reaches the
		// callback and so is not wrapped inside it) still reaches BackfillCacheSavings named.
		return fmt.Errorf("reconcile cache-savings pricing: %w", err)
	}
	return nil
}

// cacheSavingsPricedVersion reads the singleton pricing marker (cache_savings_priced_version), the
// version of the rate table the cache-savings rollups were last reconciled to. A missing singleton (a
// database before migration 0013) reads as 0, below any real version. Callers compare it to
// pricing.Version: equal means the stored rollups are priced at this binary's rates and are
// authoritative, and any difference means a pricing rollout is in flight so a stored rollup may sit at
// a different rate table than a live recompute would use.
func (s *Store) cacheSavingsPricedVersion(ctx context.Context) (int, error) {
	var priced int
	if err := s.Pool.QueryRow(ctx,
		`SELECT cache_savings_priced_version FROM parse_meta WHERE id = TRUE`).Scan(&priced); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("read pricing marker: %w", err)
	}
	return priced, nil
}

// backfillCacheSavingsForSession recomputes one candidate session's cache saving from its
// usage_events and writes it authoritatively under the session row lock, so a backfill write can
// never clobber the live parse fold. The fold (applyAggregates) increments total_cache_savings_usd
// while holding the session row lock, so this takes that lock first (SELECT ... FOR UPDATE) and
// rechecks cache_savings_backfilled under it: if a concurrent backfill already priced the session
// between the batch scan and now, it is done and this leaves the row alone. Otherwise it recomputes
// the full saving from usage_events in the same transaction (the lock blocks a concurrent fold
// from committing an interleaved increment) and writes the absolute value, replacing whatever
// partial fold the row held, then sets the flag so any later append adds only its own delta on top
// of an authoritative base. The recheck is on the flag, not the stored number: the whole point is
// that a nonzero total can be a partial fold, so "already nonzero" is not proof of correctness. It
// reports whether it wrote.
//
// It also rechecks the pricing marker under the same session lock and bows out if a newer binary has
// won. During a pricing rolling deploy an old binary at pricing.Version N-1 and a new one at N share
// the database; the new one clears cache_savings_backfilled across the corpus and advances
// cache_savings_priced_version to N. Without the recheck an old long-running drain could then lock one
// of those cleared candidates, price it at the OLD N-1 rates, and set cache_savings_backfilled=true,
// leaving a mispriced authoritative rollup a newer drain skips forever. The recheck is airtight
// because the reconcile clears the flag with an UPDATE that locks these very session rows: this
// SELECT ... FOR UPDATE serializes against that clear, so once the reconcile has committed its
// advance, an old drain that locks the row afterward reads the advanced marker and skips. If it locks
// first, it prices at N-1 and the reconcile's later clear flips the row back for the N drain to
// re-price.
func (s *Store) backfillCacheSavingsForSession(ctx context.Context, id int64) (bool, error) {
	wrote := false
	// The inner wraps name the operation; the outer wrap below adds the session id, so a begin or
	// commit failure (which never reaches the callback) is identified too, not just the query and
	// update failures inside it.
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var backfilled bool
		if err := tx.QueryRow(ctx,
			`SELECT cache_savings_backfilled FROM sessions WHERE id = $1 FOR UPDATE`, id).Scan(&backfilled); err != nil {
			return fmt.Errorf("lock session for cache-savings backfill: %w", err)
		}
		if backfilled {
			return nil // a concurrent backfill priced it after the batch scan; leave it alone
		}
		var priced int
		if err := tx.QueryRow(ctx,
			`SELECT cache_savings_priced_version FROM parse_meta WHERE id = TRUE`).Scan(&priced); err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read pricing marker for cache-savings backfill: %w", err)
			}
			// No singleton row (a database before migration 0013): nothing has priced, so no newer
			// binary can have won and priced stays 0, below any real version.
		}
		if priced > pricing.Version {
			return nil // superseded by a newer pricing binary; leave this candidate for it to price
		}
		c, err := s.sessionCacheStatsFrom(ctx, tx, id)
		if err != nil {
			return fmt.Errorf("price cache savings: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE sessions
			    SET total_cache_savings_usd = $2, cache_savings_incomplete = $3, cache_savings_backfilled = true
			  WHERE id = $1`,
			id, c.SavingsUSD, c.SavingsIncomplete); err != nil {
			return fmt.Errorf("write backfilled cache savings: %w", err)
		}
		wrote = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("backfill cache savings for session %d: %w", id, err)
	}
	return wrote, nil
}

// cacheSavingsBackfillBatch returns up to limit candidate session ids after afterID, in id
// order: cache-bearing sessions not yet marked cache_savings_backfilled. The partial index
// idx_sessions_cache_savings_candidate (WHERE NOT cache_savings_backfilled) carries only the
// candidate rows in id order, so this seeks past the id cursor over the candidates alone rather
// than scanning the whole sessions table, and a steady-state pass with no candidates is an O(1)
// index probe rather than an O(total sessions) walk. That matters because the drain runs on the
// periodic settle loop, so a full scan per tick would be quadratic as history grows. The EXISTS
// probe on usage_events is a post-filter on the (few) indexed candidates, so a candidate with no
// cache volume is dropped without a wasted price. The id cursor makes the whole backfill one
// forward walk of the candidate id space, and because backfilling flips the flag, a session is
// visited at most once even though its priced saving may legitimately be zero.
func (s *Store) cacheSavingsBackfillBatch(ctx context.Context, afterID int64, limit int) ([]int64, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT s.id
		   FROM sessions s
		  WHERE s.id > $1
		    AND NOT s.cache_savings_backfilled
		    AND EXISTS (SELECT 1 FROM usage_events u
		                 WHERE u.session_id = s.id
		                   AND (u.cache_read_tokens > 0 OR u.cache_write_tokens > 0))
		  ORDER BY s.id
		  LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("select sessions for cache-savings backfill: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan cache-savings backfill id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cache-savings backfill: %w", err)
	}
	return ids, nil
}

// cacheSavingsBackfillBatch bounds one candidate query and the run of per-session prices behind
// it. It is a var so a test can shrink it to exercise the multi-batch keyset drain.
var cacheSavingsBackfillBatch = 500
