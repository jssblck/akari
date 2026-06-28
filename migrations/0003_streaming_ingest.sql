-- Streaming incremental ingest. Replaces the whole-session re-parse path with a
-- per-chunk one: raw bytes become append-only chunk rows, the prefix hash is
-- maintained from resumable digest state, and parsing advances a byte cursor
-- instead of rebuilding the projection from scratch on every chunk.
--
-- This is a hard schema and protocol break. The raw store changes shape (the
-- single mutated BYTEA becomes append-only chunks), so every prior session would
-- have to be re-ingested to be usable. Pre-release, the cleaner guarantee is to
-- nuke session-scoped data here so nothing is left half-migrated. Identity
-- (users, tokens, invites) is untouched.

-- Reclaim large objects before dropping the rows that point at them, then clear
-- every session-scoped table. Projects go too, since they only exist to group
-- sessions. Users and auth survive.
SELECT lo_unlink(lo_oid) FROM blobs;
TRUNCATE messages, tool_calls, usage_events, attachments,
         session_raw, sessions, projects, blobs
  RESTART IDENTITY CASCADE;

-- Raw store, part 1: the parent cursor/state row. content moves out to
-- session_raw_chunks; this row keeps the length, the prefix hash, the resumable
-- hash state, and the parse cursor + serialized parser state.
ALTER TABLE session_raw DROP COLUMN content;
ALTER TABLE session_raw
  ALTER COLUMN content_sha256 SET DEFAULT 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855';
ALTER TABLE session_raw
  ADD COLUMN sha256_state        BYTEA,
  ADD COLUMN parsed_byte_len     BIGINT NOT NULL DEFAULT 0,
  ADD COLUMN parse_state_version INT    NOT NULL DEFAULT 0,
  ADD COLUMN parse_state         JSONB  NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN parse_error         TEXT   NOT NULL DEFAULT '',
  ADD CONSTRAINT session_raw_parsed_le_byte CHECK (parsed_byte_len <= byte_len);

-- Raw store, part 2: the append-only chunk rows. Each row is exactly one
-- uploaded chunk, which the client already trimmed to a newline, so every row
-- boundary is also a JSONL line boundary. byte_offset is the sequence; no
-- separate counter is needed.
CREATE TABLE session_raw_chunks (
  session_id  BIGINT NOT NULL REFERENCES session_raw(session_id) ON DELETE CASCADE,
  byte_offset BIGINT NOT NULL,
  byte_len    BIGINT NOT NULL,
  content     BYTEA  NOT NULL,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (session_id, byte_offset),
  CHECK (byte_len > 0),
  CHECK (octet_length(content) = byte_len)
);

-- messages: content_length becomes a generated column so it stays accurate for
-- free. A message is written once (the ingest protocol keeps a whole turn inside
-- one chunk, so a turn is never folded across regions), so there is no in-place
-- content rewrite and no need for an is_open flag.
ALTER TABLE messages DROP COLUMN content_length;
ALTER TABLE messages
  ADD COLUMN content_length INT GENERATED ALWAYS AS (octet_length(content)) STORED;

-- tool_calls: call_uid is the agent's own call id, so a tool result that arrives
-- on a later line is back-patched with an UPDATE keyed on it rather than by
-- carrying a growing id->row map in the parser state. Claude delivers a
-- tool_result in the following user entry, which can land in a later region, so
-- the back-patch must work across regions. The index is unique per session: a
-- call id is unique within a session, so the UPDATE touches exactly one row in
-- constant time. Uniqueness is safe because raw storage (one transaction) and
-- parsing (a separate one) are split, so a malformed duplicate id can only stall
-- that session's parse (recoverable by reparse), never fail an append.
ALTER TABLE tool_calls
  ADD COLUMN call_uid TEXT;
CREATE UNIQUE INDEX idx_tool_calls_call_uid ON tool_calls(session_id, call_uid)
  WHERE call_uid IS NOT NULL;

-- usage_events: a source-offset identity makes incremental inserts idempotent
-- even for Codex, whose usage carries no native dedup key. The unique index lets
-- a replayed line be absorbed by ON CONFLICT DO NOTHING instead of double
-- counting.
ALTER TABLE usage_events
  ADD COLUMN source_offset BIGINT,
  ADD COLUMN source_index  INT NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX idx_usage_source ON usage_events(session_id, source_offset, source_index)
  WHERE source_offset IS NOT NULL;
