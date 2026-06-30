-- The Insights page bounds every distribution to a trailing window with
--   WHERE s.started_at >= $since
-- across the grade, outcome, and archetype splits (see analytics_quality.go and
-- analytics_archetype.go). Without a started_at index that lower bound has nothing to
-- seek on, so each bounded request seq-scans the whole accumulated sessions table and
-- the work grows with total history rather than with the selected window. This is the
-- same class of cost migration 0012 fixed for usage_events.occurred_at.
--
-- This partial index seeks straight to the window's lower bound and skips the sessions
-- with no parsed start (started_at NULL), which never fall inside a window and so have
-- no place in the index. The bounded distributions become proportional to the sessions
-- in the window, not the sessions ever recorded.
--
-- The IF NOT EXISTS guard keeps this migration replayable on a database whose schema
-- already carries the index but whose schema_migrations does not record the version, as
-- happens when a dev instance is restored from a schema-only dump (the local dev-seed
-- snapshot does exactly this).
CREATE INDEX IF NOT EXISTS idx_sessions_started_at
  ON sessions(started_at)
  WHERE started_at IS NOT NULL;
