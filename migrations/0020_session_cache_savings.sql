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
-- applyDelta and filling these columns exactly as a fresh ingest would. That is the same
-- reason total_cost_usd is a parse-time fold and not a SQL sum.
ALTER TABLE sessions
    ADD COLUMN total_cache_savings_usd  DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN cache_savings_incomplete BOOLEAN          NOT NULL DEFAULT false;
