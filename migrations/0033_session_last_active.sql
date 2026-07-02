-- Feed recency that means "when the session was last active", not "when akari last
-- touched the row".
--
-- The session lists (the global Sessions view, the project view, the Overview feed)
-- displayed and ordered by updated_at, the row's last-write time. But updated_at
-- moves on every reparse and on metadata-only announce writes, so a bulk reparse
-- (an epoch bump) restamped every session to "now" and the feed showed days-old
-- transcripts as updated today. Users read "updated at" as the last time the
-- SESSION changed, not the last time akari parsed it.
--
-- last_active_at carries that meaning. It is the session's last event timestamp
-- (ended_at), which the reducer folds with GREATEST over the region's event times
-- and is therefore idempotent under a replay: a reparse reconstructs the identical
-- value, so it never drifts to "now". The COALESCE fallback to created_at covers
-- the degenerate transcript that carried no timestamps at all (ended_at NULL), so
-- the column is NOT NULL. That non-null guarantee is load-bearing: the feed order
-- relies on it to skip a NULLS placement and keep the index walk (see orderClause
-- in internal/server/store/read.go), exactly as it did for updated_at.
--
-- It is a STORED generated column so Postgres keeps it in lockstep with ended_at
-- (no trigger, no app write path to keep honest) and so a btree can walk it.
ALTER TABLE sessions
  ADD COLUMN last_active_at TIMESTAMPTZ NOT NULL
  GENERATED ALWAYS AS (COALESCE(ended_at, created_at)) STORED;

-- Feed-order indexes, moved from updated_at to last_active_at. These mirror the
-- shapes migrations 0026 (the default feed) and 0006 (the facet-filtered variants)
-- built on updated_at: the DESC, id DESC composite lets Postgres satisfy the
-- feed's "ORDER BY last_active_at DESC, id DESC LIMIT n" with an index scan that
-- stops at the page instead of sorting the whole session history. The facet
-- composites lead with the equality column (project, user, agent, machine) so a
-- filtered feed walks the index already in order, and that leading column still
-- serves the plain equality lookups the pre-0006 single-column indexes did.
CREATE INDEX idx_sessions_feed_active         ON sessions(last_active_at DESC, id DESC);
CREATE INDEX idx_sessions_project_feed_active ON sessions(project_id, last_active_at DESC, id DESC);
CREATE INDEX idx_sessions_user_feed_active    ON sessions(user_id,    last_active_at DESC, id DESC);
CREATE INDEX idx_sessions_agent_feed_active   ON sessions(agent,      last_active_at DESC, id DESC);
CREATE INDEX idx_sessions_machine_feed_active ON sessions(machine,    last_active_at DESC, id DESC);

-- The updated_at feed indexes are now dead: no query orders by updated_at anymore.
-- Drop them so the table stops carrying their write cost. IF EXISTS keeps this
-- replayable on a database that never had them (or had them dropped already).
DROP INDEX IF EXISTS idx_sessions_feed;
DROP INDEX IF EXISTS idx_sessions_project_feed;
DROP INDEX IF EXISTS idx_sessions_user_feed;
DROP INDEX IF EXISTS idx_sessions_agent_feed;
DROP INDEX IF EXISTS idx_sessions_machine_feed;
