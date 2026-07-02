-- Materialize each message's per-turn usage rollup when its usage rows are written, so the web
-- transcript reads a bounded primary-key row per message rather than re-aggregating the session's
-- whole usage_events table on every render. The session-view body fragment re-fetches on every SSE
-- append (handleSessionBody re-enters the transcript read), and the prior read grouped every
-- usage_events row of the session by message_ordinal on each of those reads: a session that grows
-- by K appends with K live refreshes made the database pay O(1 + 2 + ... + K) = O(K^2) usage
-- aggregation across the run. This table moves that fold to the one place the usage rows are already
-- being written in order (the INSERT loop inside the ordered projection apply, see projection.go),
-- accumulating each turn's totals as its usage rows land, so the render joins one indexed row per
-- message in O(1) instead of scanning and grouping the ledger.
--
-- It is a rollup OF usage_events, not a second ledger: the columns are exactly the per-ordinal fold
-- messagesFullQuery used to compute (summed token classes, the summed cost with a count of priced
-- rows so an all-unpriced turn reads nil rather than a summed zero, and a cost_incomplete flag for a
-- turn that folded a token-bearing but unpriced row). It counts only the usage rows that actually
-- persist: the projection apply folds a row here only when its ON CONFLICT insert into usage_events
-- affected a row, the same surviving set the session rollups count, so message_turn_usage reconciles
-- exactly with GROUP BY over usage_events (pinned by store's TestMessageTurnUsageMatchesUsageEvents).
-- NULL-ordinal usage rows are not attributable to a turn and are excluded, the same rule the old
-- fold applied (WHERE message_ordinal IS NOT NULL).
--
-- Like the other derived projection state it is parser-owned: reparse clears it with the rest of the
-- projection (clearProjectionForReparseTx) and the replay rebuilds it, and the paired parse.Epoch
-- bump forces that reparse across the corpus so every existing session's rollup fills in one pass.
-- A session not yet reparsed simply has no rows here and the transcript falls back to a zero turn
-- load (the LEFT JOIN yields no row), the same "unmeasured" default until the epoch reparse reaches
-- it. The session-total token invariant is untouched: this table sits beside usage_events as a
-- redundant per-turn fold, so sessions.total_* still equals sum(usage_events).
CREATE TABLE message_turn_usage (
    session_id       BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    message_ordinal  INT NOT NULL,
    input_tokens        BIGINT NOT NULL DEFAULT 0,
    output_tokens       BIGINT NOT NULL DEFAULT 0,
    cache_write_tokens  BIGINT NOT NULL DEFAULT 0,
    cache_read_tokens   BIGINT NOT NULL DEFAULT 0,
    reasoning_tokens    BIGINT NOT NULL DEFAULT 0,
    -- cost_sum is the summed cost over the turn's priced rows; cost_count is how many of those rows
    -- carried a cost. The pair lets the read apply the all-unpriced-is-nil rule: cost_count = 0 means
    -- every contributing row was unpriced, so the turn reads nil cost rather than a summed 0.0 that
    -- would misrender as free. A mixed turn keeps its priced partial (cost_sum ignores the unpriced
    -- rows) and cost_incomplete flags it as a lower bound.
    cost_sum         DOUBLE PRECISION NOT NULL DEFAULT 0,
    cost_count       BIGINT NOT NULL DEFAULT 0,
    cost_incomplete  BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (session_id, message_ordinal)
);
