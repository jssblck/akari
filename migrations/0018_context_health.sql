-- Context health records how heavy a session's context got and how often it shed that
-- context, both read from the token stream rather than from any agent-specific marker.
--
-- session_signals gains two derived figures. peak_context_tokens is the largest
-- single-turn prompt (uncached input plus cached read plus cache creation) the session
-- reached: a window-independent "how heavy did it get" score in tokens, where higher is
-- more concerning regardless of the model's actual limit. context_reset_count is the
-- number of inferred context resets, the sharp drops in that running prompt size that read
-- as a compaction or a manual clear (see quality.ContextHealth).
--
-- Both are NULL when the session has no usage to measure, so an unmeasurable session reads
-- as absent rather than as a misleading zero. Like the rest of session_signals they are
-- rebuilt from the session's own usage_events on catch-up or reparse, and they do not feed
-- the quality score: they describe resource load, not whether the session went well. They
-- are populated for already-stored sessions by the backfill reparse this commit triggers.
ALTER TABLE session_signals
    ADD COLUMN peak_context_tokens  BIGINT,
    ADD COLUMN context_reset_count  INT;

-- The two figures are always measured together: gatherContextHealth either finds no usage
-- and leaves both NULL, or computes both from the same turn sequence. The CHECK enforces
-- that pairing (both present or both absent) alongside non-negativity, so a half-populated
-- row (a measured peak with a NULL reset count, which the aggregate would read as zero)
-- cannot exist even if written by hand, the same belt-and-suspenders the hygiene subset
-- CHECK applies.
ALTER TABLE session_signals
    ADD CONSTRAINT session_signals_context_health_ck
    CHECK (
        (peak_context_tokens IS NULL) = (context_reset_count IS NULL)
        AND (peak_context_tokens IS NULL OR peak_context_tokens >= 0)
        AND (context_reset_count IS NULL OR context_reset_count >= 0)
    );
