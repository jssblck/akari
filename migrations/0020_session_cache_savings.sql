-- Roll up each session's prompt-cache dollar saving beside the token and cost totals it
-- already carries, so the session header's Cache tile reads one row instead of scanning
-- every usage_events row on each live (SSE) refresh. The saving is the gap between paying
-- the uncached input rate and the discounted cache rates for the same cached volume, priced
-- per model. The live session view re-renders that tile on every SSE update, so a long
-- session's K refreshes each rescanned its K usage rows, O(K^2) over the session. Folded
-- into the rollup at parse time the tile is O(1): it reads total_cache_savings_usd off the
-- session row the token tiles beside it already read.
--
-- total_cache_savings_usd folds exactly like total_cost_usd: applyDelta adds each surviving
-- usage row's saving (pricing.CacheSavings, priced per model) into the session total, and
-- cache_savings_incomplete rides along like cost_incomplete, set when cached volume landed on
-- a model the pricing table does not know so its saving is omitted. Unlike cost_incomplete it
-- is not a clean lower bound: an omitted model's saving can be negative (a Claude cache write
-- is priced above input, a cost paid up front), so the UI flags it "partial" rather than the
-- cost figures' "+" lower-bound marker.
--
-- The DEFAULTs seed existing rows at 0 / false. Pricing lives in the Go binary, not in SQL,
-- so this migration cannot compute the historical saving in place; the parse.Epoch bump
-- reparses the whole corpus on first deploy, re-folding every session's usage through
-- applyDelta and filling these columns exactly as a fresh ingest would.
--
-- Unlike total_cost_usd, which sums a cost the transcript stored per usage row, the saving
-- has no per-row source: it is always priced from the rate table at fold time. So a reprice
-- makes this stored per-session figure stale until a reparse, where total_cost_usd would not
-- move. That is not a new gap: AGENTS.md already requires a parse.Epoch bump on any reprice,
-- and that bump reparses the corpus and restamps this column, so the stored per-session
-- figure and the live per-model recompute (SessionCacheStats, and the analytics CacheStats
-- that reprices on every read) only diverge during the reparse the reprice already mandates.
ALTER TABLE sessions
    ADD COLUMN total_cache_savings_usd  DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN cache_savings_incomplete BOOLEAN          NOT NULL DEFAULT false;

-- cache_savings_backfilled marks a session whose rollup is the authoritative full fold of its
-- usage_events, so BackfillCacheSavings can find the ones that are not without inferring it from
-- the stored number. A zero or nonzero total is not proof: a session seeded at 0 here that then
-- takes a live append folds only the new rows, leaving a partial nonzero total, and if the epoch
-- reparse fails that partial value sticks. Keying the backfill on this flag instead of "total is
-- zero" catches that case, and its authoritative recompute repairs it.
--
-- It defaults true, so a session ingested after this migration (which folds its whole usage from
-- a correct empty base) is authoritative from creation and never a backfill candidate. Every
-- session that predates the column is seeded at a suspect 0, so the UPDATE marks the cache-bearing
-- ones false: the epoch reparse re-folds them and the backfill is the safety net for any reparse
-- that fails. A reset (a reparse in flight) deliberately leaves this flag alone, so a reparse that
-- rolls back does not turn an authoritative session back into a candidate.
ALTER TABLE sessions
    ADD COLUMN cache_savings_backfilled BOOLEAN NOT NULL DEFAULT true;

UPDATE sessions
   SET cache_savings_backfilled = false
 WHERE EXISTS (SELECT 1 FROM usage_events u
                WHERE u.session_id = sessions.id
                  AND (u.cache_read_tokens > 0 OR u.cache_write_tokens > 0));
