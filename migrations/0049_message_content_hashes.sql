-- Authenticated MCP references bind to the exact message field they described.
-- Store the digests with the projection so serving a preview never has to pull an
-- oversized field into application memory merely to hash it. A trigger covers
-- direct inserts and every rebuild path. PostgreSQL does not consider the text to
-- bytea conversion immutable, so these cannot be generated columns.
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

UPDATE messages
   SET content_sha256 = encode(sha256(convert_to(content, 'UTF8')), 'hex'),
       thinking_text_sha256 = encode(sha256(convert_to(thinking_text, 'UTF8')), 'hex');
