-- Authenticated MCP references bind to the exact message field they described.
-- Store the digests with the projection so serving a preview never has to pull an
-- oversized field into application memory merely to hash it. A trigger covers
-- direct inserts and every rebuild path. PostgreSQL does not consider the text to
-- bytea conversion immutable, so these cannot be generated columns.
--
-- This migration deliberately stops at the column and the trigger: it does not
-- backfill the existing corpus here. A full-table UPDATE in the same transaction
-- as the ALTER would hold the ADD COLUMN's access-exclusive lock on messages, the
-- hottest table, for as long as the backfill took, stalling startup and blocking
-- live traffic on any sizable corpus (see migration 0041 for the established
-- alternative of deferring bulk population out of the migration transaction). The
-- default '' sentinel lets existing rows add the column instantly; a bounded
-- background pass at server startup (Store.BackfillMessageContentHashes, wired in
-- cmd/akari-server) fills them in afterward, and every reader that consumes these
-- columns tolerates the sentinel until then (see internal/server/store/read_mcp_messages.go).
ALTER TABLE messages
  ADD COLUMN content_sha256 CHAR(64) NOT NULL DEFAULT '',
  ADD COLUMN thinking_text_sha256 CHAR(64) NOT NULL DEFAULT '';

CREATE FUNCTION stamp_message_content_hashes() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  NEW.content_sha256 := encode(sha256(convert_to(NEW.content, 'UTF8')), 'hex');
  NEW.thinking_text_sha256 := encode(sha256(convert_to(NEW.thinking_text, 'UTF8')), 'hex');
  RETURN NEW;
END;
$$;

CREATE TRIGGER messages_content_hashes
BEFORE INSERT OR UPDATE OF content, thinking_text ON messages
FOR EACH ROW EXECUTE FUNCTION stamp_message_content_hashes();
