-- Pricing is a best-effort estimate throughout Akari. Unknown model rates use
-- zero, so the database no longer needs a parallel completeness state.
UPDATE usage_events SET cost_usd = 0 WHERE cost_usd IS NULL;

ALTER TABLE usage_events
  ALTER COLUMN cost_usd SET DEFAULT 0,
  ALTER COLUMN cost_usd SET NOT NULL;

ALTER TABLE sessions
  DROP COLUMN IF EXISTS cost_incomplete,
  DROP COLUMN IF EXISTS cache_savings_incomplete;

ALTER TABLE message_turn_usage
  DROP COLUMN IF EXISTS cost_count,
  DROP COLUMN IF EXISTS cost_incomplete;

ALTER TABLE session_usage_daily
  DROP COLUMN IF EXISTS unpriced;
