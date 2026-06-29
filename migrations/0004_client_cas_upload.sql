-- Client-side CAS upload. The client now lifts every tool input and result body
-- out of the transcript, uploads each to the content-addressed store directly,
-- and uploads the transcript with each body replaced by a compact sentinel. The
-- server records the reference instead of re-storing the body, so a giant tool
-- output never travels inline and the transcript stays small at any size.
--
-- This changes the on-wire and on-disk transcript format (bodies become
-- sentinels), so prior sessions must be re-ingested to be readable. Pre-release,
-- the clean guarantee is to clear session-scoped data here rather than leave it
-- half-converted. Identity (users, tokens, invites) is untouched.

-- Reclaim large objects before dropping the rows that point at them, then clear
-- every session-scoped table. Projects go too, since they only group sessions.
SELECT lo_unlink(lo_oid) FROM blobs;
TRUNCATE messages, tool_calls, usage_events, attachments,
         session_raw_chunks, session_raw, sessions, projects, blobs
  RESTART IDENTITY CASCADE;

-- Sweep-protection pins. A body the client has uploaded but not yet referenced
-- from a transcript would otherwise look orphaned to the background sweep. A pin
-- protects it for a TTL: PutBlob inserts (or refreshes) a pin on upload, the
-- sweep excludes any blob with a live pin, and expired pins are cleared at the
-- start of each sweep so a body whose transcript never arrived is reclaimable.
--
-- ON DELETE CASCADE keeps the pin from outliving its blob: the sweep clears
-- expired pins before computing orphans, so it never deletes a still-pinned blob,
-- but the cascade is a belt-and-suspenders guard against a stranded pin row.
CREATE TABLE blob_pins (
  sha256     CHAR(64) PRIMARY KEY REFERENCES blobs(sha256) ON DELETE CASCADE,
  expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_blob_pins_expires ON blob_pins(expires_at);
