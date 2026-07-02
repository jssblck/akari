-- Materialize each user turn's duplicate-prompt verdict when the message is written, so the web
-- transcript reads a stored boolean rather than recomputing a whole-session window on every render.
-- The transcript's live body fragment re-fetches on every SSE append, and the prior approach folded
-- the duplicate flag with a window function over every message in the session on each of those
-- reads: a session that grows by K appends then paid O(K) accumulated whole-session work per
-- refresh. This column moves the verdict to the one place the prior prefix is already known and
-- ordered (the INSERT that writes the row, inside the ordered projection apply), so the render reads
-- it in O(1) per row.
--
-- The store fills it in projection.go: a user turn is a duplicate when it is duplicate-eligible
-- (non-empty content, current-version prompt facts, a non-null digest, and NOT prompt_short) AND an
-- earlier eligible user row in the same session already carries the same prompt_digest. Messages
-- apply in ordinal order within a transaction, and the reparse re-inserts them in that same order,
-- so the prior-prefix existence check the insert runs sees exactly the earlier eligible rows the
-- old window counted, over the same eligible set gatherPromptHygiene's duplicate_prompt_count uses.
-- The first occurrence of a digest reads false (the original); every later eligible occurrence reads
-- true. Ineligible rows (assistant turns, short prompts, empty content, superseded facts) stay NULL.
--
-- Like the prompt-hygiene facts (migration 0022) this does not backfill on ALTER: existing rows read
-- NULL until reparsed. The paired parse.Epoch bump forces that reparse across the corpus, re-deriving
-- the flag in one ordered pass; until then a not-yet-reparsed session reads the flag as absent and
-- the transcript simply shows no repeat badge, the same "unmeasured" default the hygiene facts use.
ALTER TABLE messages
    ADD COLUMN duplicate_prompt BOOLEAN;

-- The insert's duplicate check probes for an earlier eligible user row in the same session carrying
-- this prompt_digest. This partial index serves that probe as a bounded lookup rather than a scan of
-- the session's messages, so materializing the flag stays linear on the append path: each appended
-- eligible user message costs one index probe, not an O(session) rescan that would make ingest
-- quadratic. It covers only the duplicate-eligible rows the probe reads (role='user', a non-null
-- digest, not short), keeping it small.
CREATE INDEX idx_messages_session_digest
    ON messages (session_id, prompt_digest)
    WHERE role = 'user' AND prompt_digest IS NOT NULL AND NOT coalesce(prompt_short, false);
