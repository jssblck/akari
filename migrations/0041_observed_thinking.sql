-- Observed thinking: how much the model actually deliberated in a session, measured from
-- the reasoning trace the agents log, not from any configured "thinking level" (no agent's
-- transcript records the setting uniformly). The primitive is the per-turn reasoning
-- volume; the session scalars stored here summarize a session's turns, and the
-- off/low/medium/high/xhigh label is an absolute band on an estimated-token scale computed
-- at read time (see internal/quality/thinking.go), shared by the per-message badge, the
-- session readout, and the fleet distribution.
--
-- messages.thinking_bytes is the per-message reasoning-trace weight the parser records (see
-- parser.Message.ThinkingBytes): the reasoning plaintext length where the agent logs it, else
-- the encrypted payload length. Current Claude Code and Codex ship the reasoning encrypted
-- (Claude leaves a "signature", Codex an "encrypted_content" blob) and drop the plaintext, so
-- thinking_text is empty while the reasoning still happened; the ciphertext length tracks the
-- hidden reasoning volume closely (r=0.97 for Claude signatures, r=0.997 for Codex against the
-- reasoning-token count it reports). Because the weight is parser-computed rather than derived
-- from a column, it is populated by a reparse: this migration pairs with a parse.Epoch bump
-- that reparses the corpus and fills thinking_bytes on every message. The DEFAULT 0 lets the
-- column add without a table rewrite; the reparse then overwrites it per row. Codex's exact
-- reasoning-token count per turn is read from message_turn_usage.reasoning_tokens (migration
-- 0032) at derive time, so it needs no column here.
ALTER TABLE messages
  ADD COLUMN thinking_bytes INT NOT NULL DEFAULT 0;

-- Per-session observed-thinking scalars, derived by the settle pass beside the other signal
-- groups. Like context health they are informational: they never feed the quality score. They
-- summarize the session's assistant turns on the estimated-token scale (each turn's exact
-- reasoning tokens where the agent reports them, else thinking_bytes divided by the agent's
-- calibrated bytes-per-token):
--
--   assistant_turns       the denominator: the session's assistant messages.
--   thinking_turns         how many carried a reasoning block (has_thinking). Zero is "off".
--   thinking_tail_tokens   the session's headline volume: the mean of the hardest tenth of its
--                          thinking turns (ceil(thinking_turns / 10) turns). A tail statistic,
--                          not a mean over all turns: most turns barely think, so an all-turn
--                          average collapses to the floor, while the hardest-decile mean reads
--                          "how hard it thought when it thought hard" without a single outlier
--                          turn defining the session the way a bare max would.
--   thinking_peak_tokens   the single hardest turn's volume, the tail's companion.
--
-- The scale is absolute and agent-independent: Codex reasoning tokens are exact, and the
-- Claude/pi byte estimates land on the same token scale (Claude-estimated and Codex-exact
-- medians agree within ~2%), so a token figure is comparable across models without the
-- per-model ranking the first cut used.
--
-- All NULL when the session has no assistant turns at all (nothing to measure), so an
-- unmeasurable session reads as absent rather than as "off".
ALTER TABLE session_signals
  ADD COLUMN assistant_turns      INT,
  ADD COLUMN thinking_turns       INT,
  ADD COLUMN thinking_tail_tokens INT,
  ADD COLUMN thinking_peak_tokens INT;

-- The four figures are one measurement: either the session had assistant turns and all four
-- are present, or it had none and all four are NULL. Within a measurement the thinking turns
-- are a subset of the assistant turns; a session with no thinking turns carries no volume; and
-- the peak turn is at least the hardest-decile mean (a max is never below a mean over the top
-- of the same set), so peak >= tail always holds.
ALTER TABLE session_signals
  ADD CONSTRAINT session_signals_thinking_ck CHECK (
    ((assistant_turns IS NULL) = (thinking_turns IS NULL))
    AND ((assistant_turns IS NULL) = (thinking_tail_tokens IS NULL))
    AND ((assistant_turns IS NULL) = (thinking_peak_tokens IS NULL))
    AND (assistant_turns IS NULL OR (
      assistant_turns > 0
      AND thinking_turns BETWEEN 0 AND assistant_turns
      AND thinking_tail_tokens >= 0
      AND thinking_peak_tokens >= thinking_tail_tokens
      AND (thinking_turns > 0 OR thinking_peak_tokens = 0)
    ))
  );
