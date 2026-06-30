-- Sort-order indexes for the global Sessions view's click-to-sort headers.
--
-- PR #35 made seven columns sortable. The feed indexes (0004, 0006) cover only
-- the default order (updated_at DESC, id DESC) and the facet-filtered variants of
-- it, so a click on any OTHER column fell back to sorting the whole accumulated
-- session history per request to surface one 100-row page. These indexes restore
-- the index-walk discipline for the columns that are session-local: an ordered
-- page becomes an index scan that stops at the LIMIT instead of a full sort.
--
-- The id tiebreak follows the column's sort direction (see SessionFilter.orderClause),
-- so each (col, id) btree serves BOTH directions reachable from the UI: a forward
-- scan satisfies "col ASC, id ASC" and a backward scan "col DESC, id DESC". One
-- index per column therefore covers the asc/desc header toggle.
--
-- tokens sorts on the sum of the four token classes. A running sum is not a column
-- an index can walk, so total_tokens is added as a STORED generated column (kept in
-- lockstep with its inputs by Postgres) and indexed. It doubles as the canonical
-- "all tokens" figure the session detail and project rollups already compute by hand.
--
-- project and user sorts are deliberately NOT indexed here: project sorts on a CASE
-- over the joined projects table and user on the joined users.username, neither a
-- session column. Walking them to the LIMIT would mean denormalizing the sort key
-- onto every session (heavy, and stale on a project rename), so those two rank the
-- working set instead. That set is bounded by any active facet filter, and only the
-- unfiltered whole-history case is a full sort, which is cheap at a single team's
-- session volume.
--
-- agent and git_branch are effectively immutable once a session exists, so their
-- indexes cost only an insert. message_count and total_tokens change on every ingest
-- append, so their indexes carry that update cost, the same posture the updated_at
-- feed indexes already take.

-- The IF NOT EXISTS guards keep this migration replayable on a database whose
-- schema already carries these objects but whose schema_migrations does not record
-- the version, as happens when a dev instance is restored from a schema-only dump
-- (the local dev-seed snapshot does exactly this). The runner keys on the version
-- string alone, so a clean database still applies this once and is unaffected.
ALTER TABLE sessions
  ADD COLUMN IF NOT EXISTS total_tokens BIGINT NOT NULL
  GENERATED ALWAYS AS (
    total_input_tokens + total_output_tokens
    + total_cache_read_tokens + total_cache_write_tokens
  ) STORED;

CREATE INDEX IF NOT EXISTS idx_sessions_agent_sort    ON sessions(agent, id);
CREATE INDEX IF NOT EXISTS idx_sessions_branch_sort   ON sessions(git_branch, id);
CREATE INDEX IF NOT EXISTS idx_sessions_messages_sort ON sessions(message_count, id);
CREATE INDEX IF NOT EXISTS idx_sessions_tokens_sort   ON sessions(total_tokens, id);
