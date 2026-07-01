-- cache_savings_backfilled marks a session whose per-session cache-savings rollup is authoritative
-- at the current pricing.Version, the cache analog of signals_stale. The cache-savings drain
-- (BackfillCacheSavings, cacheSavingsBackfillBatch) selects the sessions that still need pricing by
-- an index seek rather than re-examining the whole sessions table on every pass.
--
-- The drain now runs on the periodic settle loop (runSettleMaintenance), not just once at startup,
-- so that a pricing rolling deploy's candidates are consumed each tick rather than lingering until a
-- read reprices them. That makes the steady-state cost matter: the drain keyset-pages candidates by
--   WHERE id > $1 AND NOT cache_savings_backfilled AND EXISTS (cache usage) ORDER BY id
-- and without a partial index on the flag, proving the candidate set is empty means walking the whole
-- sessions id space to check the flag on every row, O(total sessions) per tick and O(N^2) across ticks
-- as history grows. That is the same quadratic the signals settle pass avoids with
-- idx_sessions_signals_stale (see migration 0021).
--
-- This partial index carries only the candidate rows (NOT cache_savings_backfilled) in id order, so
-- the drain seeks past its id cursor over the candidates alone and an empty candidate set is an O(1)
-- probe, never a full scan. A session enters the index when applyAggregates or the pricing reconcile
-- clears the flag (a superseded rate table left its rollup provisional) and leaves it when the
-- backfill re-prices it and sets the flag true. The default is true (a fresh session is priced
-- authoritatively by the live fold), so the index stays small: it holds only sessions actively
-- awaiting a reprice, not the whole corpus. IF NOT EXISTS keeps it replayable on a schema-only dev
-- dump that already carries the index.
CREATE INDEX IF NOT EXISTS idx_sessions_cache_savings_candidate
  ON sessions (id)
  WHERE NOT cache_savings_backfilled;
