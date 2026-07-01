-- Prompt-hygiene signals extend session_signals with input-quality counts: how many of
-- a session's human prompts were terse, repeated verbatim, or asked for a change while
-- pointing at no code, and whether the session opened with an unstructured prompt. They
-- describe the human's input rather than the agent's behavior, so they sit beside the
-- tool-health counts but do not feed the quality score (an unclear prompt is not the
-- agent's fault). Like the rest of session_signals they are derived and self-healing:
-- rebuilt from the session's own messages on catch-up or reparse, never part of the
-- token-rollup invariant.
--
-- The columns default to zero/false so rows written before this migration read as
-- "no hygiene signal" until their backfill reparse (the parse.Epoch bump in this commit)
-- recomputes them at the current signals version.
ALTER TABLE session_signals
    -- prompt_count is the classifier's base: the session's human prompts with non-empty
    -- content, the exact set the hygiene counts are drawn from. The cohort aggregate uses
    -- it as the rate denominator so a numerator can never exceed it, even for an agent
    -- (Codex, Pi) whose user turns may carry an empty-text, image-only body that
    -- user_message_count would count but the classifier never saw.
    ADD COLUMN prompt_count           INT     NOT NULL DEFAULT 0,
    ADD COLUMN short_prompt_count     INT     NOT NULL DEFAULT 0,
    ADD COLUMN duplicate_prompt_count INT     NOT NULL DEFAULT 0,
    ADD COLUMN no_code_context_count  INT     NOT NULL DEFAULT 0,
    ADD COLUMN unstructured_start     BOOLEAN NOT NULL DEFAULT false;

-- Each hygiene count is drawn from the prompt_count base, so none can exceed it. The
-- deriving code preserves that by construction (every count is over a subset of the
-- prompts); the CHECK makes the database enforce it too, so a future classifier bug that
-- broke the invariant would fail loudly on write rather than quietly skewing a rate.
ALTER TABLE session_signals
    ADD CONSTRAINT session_signals_hygiene_ck
    CHECK (
        prompt_count >= 0
        AND short_prompt_count BETWEEN 0 AND prompt_count
        AND duplicate_prompt_count BETWEEN 0 AND prompt_count
        AND no_code_context_count BETWEEN 0 AND prompt_count
    );
