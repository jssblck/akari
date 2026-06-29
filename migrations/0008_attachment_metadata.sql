-- Attachments become a parser-owned projection. The reducer now lifts the binary
-- images Codex inlines (image_generation results, pasted images in a message or a
-- user_message event) into the CAS and records each as an attachment, so the row needs
-- the same size and media metadata a tool body carries, and a uniqueness key so a
-- replayed chunk or a reparse cannot double-insert the same image.
--
-- media_type is the image's semantic type (image/png, image/jpeg, ...), so the UI can
-- render it without fetching the blob. byte_len is the raw (decoded) size, matching the
-- bytes the sentinel records. Both default for any pre-existing row, though there are
-- none: attachments was never populated before this change.
ALTER TABLE attachments
  ADD COLUMN media_type TEXT NOT NULL DEFAULT 'application/octet-stream',
  ADD COLUMN byte_len   BIGINT NOT NULL DEFAULT 0;

-- One image per (session, message, content hash). The reducer already dedups within a
-- region, and this makes the insert idempotent across a replayed region or a reparse:
-- the same image attached to the same message is recorded once. message_ordinal is set
-- on every parser-emitted attachment (never NULL), so the key is total in practice.
CREATE UNIQUE INDEX idx_attachments_session_ordinal_sha
  ON attachments (session_id, message_ordinal, sha256);

-- A blob lookup keyed on the content hash. Two hot paths need it, and the unique index
-- above cannot serve either because its leading columns are session_id then
-- message_ordinal, leaving sha256 unindexed as a prefix:
--   - serving a blob authorizes it with SessionReferencesBlob (session_id = $1 AND
--     sha256 = $2), so rendering N attachments runs N such lookups; without this index
--     each one scans every attachment in the session, making a page O(N^2).
--   - the orphan sweep tests each blob with EXISTS (SELECT 1 FROM attachments WHERE
--     sha256 = b.sha256), a lookup on the hash alone.
-- Leading with sha256 serves the sweep's hash-only probe and, with session_id second,
-- the authorization probe's two-column equality.
CREATE INDEX idx_attachments_sha_session
  ON attachments (sha256, session_id);
