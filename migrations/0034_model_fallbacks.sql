-- Record when a Claude Fable turn was declined by the safety classifier and re-served by a
-- lower model (a "model fallback"), so the feed and the session header can highlight sessions
-- that fell back. Detection keys ONLY on explicit Claude Code markers the parser reads: a
-- "fallback" content block, a usage.iterations "fallback_message" entry, or a
-- "model_refusal_fallback" system entry. A bare model-string change is never a fallback: an
-- intentional switch (a /model command, a resume under a different default, a subagent on a
-- smaller model) leaves no marker and produces no row here.
--
-- One logical fallback surfaces across several JSONL lines. Claude splits one API message into
-- several assistant entries sharing the requestId (each repeating the same usage.iterations), and
-- a separate system entry carries the refusal category. The parser emits an op per line and the
-- store merges them by (session_id, dedup_key): the assistant side brings message_ordinal and the
-- declined attempt's token counts, the system side brings trigger, category, and explanation, and
-- the merge fills each column from whichever line carried it (non-empty wins over empty, non-null
-- over null, an ordinal or token count over its unfilled default). dedup_key is the top-level
-- requestId when present, else the assistant message id.
--
-- Like the other parser-owned projection state it is rebuilt by reparse: clearProjectionForReparseTx
-- deletes these rows and resets sessions.model_fallback_count with the rest of the projection, and
-- the paired parse.Epoch bump forces that reparse across the corpus so existing sessions detect their
-- fallbacks in one pass. The count sits outside the sessions.total_* == sum(usage_events) invariant:
-- it is a count of distinct fallback events, not a token rollup.
CREATE TABLE model_fallbacks (
    session_id                 BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    -- The message the assistant-side op produced (the fallback's served turn). NULL on a row that
    -- only ever saw the system entry, which produces no message row.
    message_ordinal            INT NULL,
    from_model                 TEXT NOT NULL DEFAULT '',
    to_model                   TEXT NOT NULL DEFAULT '',
    -- trigger, refusal_category, and refusal_explanation come only from the system entry, so a row
    -- built from the assistant side alone leaves them at their empty/NULL default until the system
    -- entry's op merges in (or stays there if that entry never arrived). apiRefusalCategory and
    -- apiRefusalExplanation are nullable in the transcript, so the columns are nullable here.
    trigger                    TEXT NOT NULL DEFAULT '',
    refusal_category           TEXT NULL,
    refusal_explanation        TEXT NULL,
    -- The declined attempt's token counts, summed over the usage.iterations type=="message" entries
    -- on the assistant side. NULL until that side merges in (a system-only row never carries them).
    declined_input_tokens      INT NULL,
    declined_output_tokens     INT NULL,
    declined_cache_write_tokens INT NULL,
    declined_cache_read_tokens  INT NULL,
    occurred_at                TIMESTAMPTZ NULL,
    dedup_key                  TEXT NOT NULL,
    PRIMARY KEY (session_id, dedup_key)
);

-- The per-session count of distinct fallback events, folded on first insert of each dedup_key (a
-- later merge into the same key does not re-count). It rides the sessions row so the feed and the
-- session header read it in O(1) rather than counting model_fallbacks per render, and reparse resets
-- it to 0 alongside the other rollups.
ALTER TABLE sessions
  ADD COLUMN model_fallback_count INT NOT NULL DEFAULT 0;
