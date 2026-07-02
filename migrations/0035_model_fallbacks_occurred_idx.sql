-- Make SessionModelFallbacks's ordered read seekable. It reads one session's rows ordered by
-- (occurred_at, dedup_key) and stops at a LIMIT (the capped first N by occurrence, see
-- store.ModelFallbackListCap). model_fallbacks' primary key is (session_id, dedup_key), which
-- orders by dedup_key, not occurrence, so without a matching index Postgres fetches every fallback
-- for the session and sorts it on each call. That read rides sessionHeaderStats, which re-runs on
-- every SSE refresh of a session with fallbacks, so a live repeated-fallback session paid O(F) per
-- refresh and O(F^2) across a run. This index lets the read walk in occurrence order and stop at the
-- LIMIT, bounding the work by the cap rather than the session's total fallback count. dedup_key is
-- the tiebreaker so a row with a NULL occurred_at (a system-only row that never got a timestamp)
-- still orders deterministically.
--
-- Split from 0034 (which creates the table) into its own migration so it lands on a database that
-- already applied 0034 before this fix: the runner records migrations by version and never re-runs
-- an applied one.
CREATE INDEX IF NOT EXISTS model_fallbacks_session_occurred_idx
    ON model_fallbacks (session_id, occurred_at, dedup_key);
