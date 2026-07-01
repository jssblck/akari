-- Drop the is_sidechain column an earlier cut of 0018 added on this branch. A subagent
-- turns out to be a separate transcript file that akari ingests as its own session, so a
-- main session's usage never carried subagent turns and the flag guarded a case that never
-- occurred. 0018 no longer adds the column, but a database that applied the earlier 0018
-- still has it (the runner tracks migrations by name, so an edit to an applied file does not
-- re-run); IF EXISTS makes this a no-op on a clean database and a drop on an old one.
ALTER TABLE usage_events DROP COLUMN IF EXISTS is_sidechain;

-- Link every already-ingested Claude subagent session to the session that spawned it.
--
-- A subagent runs in its own transcript file that the client nests under the parent's
-- source id ("<parent>/subagents/..."), and akari ingests it as its own session. The
-- schema has modeled the parent link since 0001 (parent_session_id, relationship_type),
-- but nothing wrote it, so those child sessions floated as top-level rows. The ingest path
-- now records the link on announce; this one-time backfill adopts the rows stored before
-- that, matching each child's source-id prefix (the part before "/subagents/") to a parent
-- session of the same user. New sessions link on announce, so this never needs to run again.
UPDATE sessions AS child
   SET parent_session_id = parent.id,
       relationship_type = 'subagent'
  FROM sessions AS parent
 WHERE child.agent = 'claude'
   AND child.parent_session_id IS NULL
   AND position('/subagents/' IN child.source_session_id) > 1
   AND parent.user_id = child.user_id
   AND parent.agent = 'claude'
   AND parent.source_session_id = split_part(child.source_session_id, '/subagents/', 1);

-- Support the adopt-children lookup that runs on every top-level Claude announce. It matches
-- children by the parent-source expression split_part(source_session_id, '/subagents/', 1),
-- so the index is on that expression: equality on it stays index-served under pgx's cached
-- generic plans, where a parameterized LIKE prefix would not. The partial predicate keeps
-- the index to the rows an adopt can still touch (unlinked Claude sessions), and a subagent
-- drops out of it the moment it links.
CREATE INDEX idx_sessions_unlinked_subagents
    ON sessions (user_id, split_part(source_session_id, '/subagents/', 1))
    WHERE agent = 'claude' AND parent_session_id IS NULL;
