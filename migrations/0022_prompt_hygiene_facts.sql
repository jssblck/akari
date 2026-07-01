-- Materialize each human prompt's hygiene facts once, when the message is written, so the settle
-- pass computes the prompt-hygiene signals from fixed-size columns rather than reading prompt bodies
-- back and re-classifying them. The prior approaches all made peak memory track the largest prompt: a
-- Go fold read every body into the heap on each settle, and a SQL generated digest re-normalized
-- (lower + regexp_replace) each body inside Postgres on every insert. These columns move the work to
-- the one place the body is already resident (the INSERT that writes the row) and keep only the
-- verdicts: three booleans and a 64-bit digest per user message.
--
-- The store fills them with quality.ClassifyPrompt (see the message insert in projection.go): a
-- bounded, allocation-free classification plus a streaming FNV digest that never builds a normalized
-- copy of the body. Only real human turns (role='user', non-empty content) carry facts; other rows
-- leave them NULL, and the hygiene aggregate reads role='user' only.
--
--   prompt_short          the prompt fell under the terse word threshold.
--   prompt_no_code        it asked for a code change while pointing at no code.
--   prompt_bare_greeting  it was only greeting and pleasantry words (an unstructured opener).
--   prompt_digest         a normalized fingerprint (lowercase, whitespace collapsed); the session
--                         duplicate count is count(*) - count(DISTINCT prompt_digest) over the
--                         duplicate-eligible (non-short) prompts.
--   prompt_facts_version  the quality.PromptFactsVersion the facts above were classified under, so a
--                         later change to ClassifyPrompt's rules is told apart from the current ones.
--                         The settle pass treats a row at an older version like an unfilled one (see
--                         gatherPromptHygiene): it leaves the session ungraded until the reparse
--                         re-derives the facts, so a hygiene count never mixes classifier versions.
--
-- Unlike a generated column these do not backfill on ALTER: existing message rows read NULL until the
-- session is reparsed. The paired parse.Epoch bump (2 -> 3) forces exactly that reparse across the
-- corpus, re-inserting every message through ClassifyPrompt so the columns fill in one pass, while a
-- still-settling session reads its hygiene signal as unmeasured until then.
ALTER TABLE messages
    ADD COLUMN prompt_short BOOLEAN,
    ADD COLUMN prompt_no_code BOOLEAN,
    ADD COLUMN prompt_bare_greeting BOOLEAN,
    ADD COLUMN prompt_digest BIGINT,
    ADD COLUMN prompt_facts_version INT;
