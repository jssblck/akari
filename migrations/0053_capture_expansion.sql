-- Widen the projection to the transcript data the parsers now read (paired with
-- the parse.Epoch bump that rebuilds the corpus into it): session-level identity
-- the transcript declares about itself, tool-call attribution and the structured
-- result body, a generic session-event stream, and transcript-declared subagent
-- parenthood.

-- Session identity: all last-write-wins scalars the rebuild copies from the
-- parsed delta. Empty means the transcript never stated the field, so readers can
-- fall back (the title lateral coalesces custom_title over the first prompt).
ALTER TABLE sessions
  ADD COLUMN custom_title     TEXT NOT NULL DEFAULT '',
  ADD COLUMN slug             TEXT NOT NULL DEFAULT '',
  -- The loosest statement of what the agent could do unasked: Claude's permission
  -- mode ("bypassPermissions", ...) or Codex's sandbox policy type
  -- ("danger-full-access", ...).
  ADD COLUMN permission_mode  TEXT NOT NULL DEFAULT '',
  ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT '',
  -- The role a subagent transcript played for its parent: Claude's agent type
  -- ("Explore"), the last segment of Codex's agent_path.
  ADD COLUMN subagent_name    TEXT NOT NULL DEFAULT '',
  ADD COLUMN pr_number        INT NOT NULL DEFAULT 0,
  ADD COLUMN pr_url           TEXT NOT NULL DEFAULT '',
  ADD COLUMN pr_repo          TEXT NOT NULL DEFAULT '';

-- Subagent parenthood becomes a stored column instead of a source-id string
-- expression, because Codex declares it inside the transcript
-- (session_meta.parent_thread_id, written by the rebuild) while Claude encodes it
-- in the source id (written by announce). One column serves both: link-up and
-- adoption key on it, whichever side learned it first.
ALTER TABLE sessions
  ADD COLUMN parent_source_id TEXT NOT NULL DEFAULT '';

-- Claude parenthood is derivable from the source id, so existing rows fill in one
-- pass; Codex rows fill as the epoch-forced rebuild parses each transcript's
-- session_meta.
UPDATE sessions
   SET parent_source_id = split_part(source_session_id, '/subagents/', 1)
 WHERE agent = 'claude'
   AND position('/subagents/' IN source_session_id) > 1;

-- The adopt-children lookup (a parent announce or rebuild claiming children
-- ingested first) probes unlinked rows by the declared parent source. Replaces
-- idx_sessions_unlinked_subagents, whose split_part expression only ever matched
-- the Claude encoding.
CREATE INDEX idx_sessions_unlinked_children
    ON sessions (user_id, agent, parent_source_id)
    WHERE parent_session_id IS NULL AND parent_source_id <> '';
DROP INDEX idx_sessions_unlinked_subagents;

-- Tool-call attribution: which subagent type, invoked skill, and plugin drove the
-- line that issued the call (Claude stamps all three independently; MCP
-- attribution is dropped as redundant with the mcp__<server>__<tool> name).
ALTER TABLE tool_calls
  ADD COLUMN attribution_agent  TEXT NOT NULL DEFAULT '',
  ADD COLUMN attribution_skill  TEXT NOT NULL DEFAULT '',
  ADD COLUMN attribution_plugin TEXT NOT NULL DEFAULT '',
  -- The structured result body (Claude's top-level toolUseResult), a CAS
  -- reference beside the flattened text result. All three NULL when the agent
  -- logged none, matching the pending-result convention of the result_* columns.
  ADD COLUMN struct_sha256     CHAR(64) REFERENCES blobs(sha256),
  ADD COLUMN struct_bytes      BIGINT,
  ADD COLUMN struct_media_type TEXT;

-- The struct reference joins blob authorization and the orphan sweep exactly like
-- the input/result references, so it takes the same hash-leading index (see 0009).
CREATE INDEX idx_tool_calls_struct_sha_session
  ON tool_calls (struct_sha256, session_id);

-- Notable non-message occurrences a transcript records: context compactions,
-- turn duration telemetry, aborted turns, API errors, stop-hook summaries,
-- subagent lifecycle events, pi model/thinking-level changes. One table with a
-- kind column rather than a table per kind, because every kind shares the same
-- shape: an optional message anchor, a timestamp, and a small bag of scalars
-- nothing joins on (attrs, one JSON object per kind, documented on the parser's
-- Event* constants). Parser-owned projection state: the rebuild deletes and
-- reinserts a session's rows, seq numbering them in transcript order.
CREATE TABLE session_events (
    session_id      BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    seq             INT NOT NULL,
    message_ordinal INT NULL,
    kind            TEXT NOT NULL,
    attrs           JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at     TIMESTAMPTZ NULL,
    PRIMARY KEY (session_id, seq)
);
