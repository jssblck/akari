-- session_signals holds per-session derived behavioral signals, all rebuilt from a
-- session's own projection (its messages, tool_calls, and usage_events) whenever the
-- session catches up to its stored bytes or is reparsed. Being derived, it never
-- participates in the token-rollup invariant (sessions.total_* == sum over
-- usage_events): a stale or missing row self-heals on the next rebuild rather than
-- being a correctness bug. ON DELETE CASCADE ties each row to its session, so the parse
-- and reset paths that clear projection rows drop this row too.
--
-- The columns fall in three groups, every row stamped with the quality.Version that
-- computed it:
--   * Outcome and tool health: an outcome classification (completed / abandoned /
--     errored / unknown, with a confidence), a 0-100 quality score and its A-F grade,
--     and the tool-health counts the score is built from. score and grade are NULL for
--     an unscored session (an unknown outcome with no tool signal carries no meaningful
--     grade, the restraint agentsview shows rather than grading an empty blip).
--   * Prompt hygiene: input-quality counts describing the human's prompts (terse,
--     repeated verbatim, or asking for a change while pointing at no code) and whether
--     the session opened unstructured. They sit beside the tool-health counts but do
--     not feed the score: an unclear prompt is not the agent's fault.
--   * Context health: how heavy the session's context got (peak_context_tokens, the
--     largest single-turn prompt in tokens) and how often it shed that context
--     (context_reset_count, the inferred compactions or clears; see
--     quality.ContextHealth). Both are NULL when the session has no usage to measure,
--     so an unmeasurable session reads as absent rather than a misleading zero.
CREATE TABLE session_signals (
    session_id             BIGINT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    -- The quality.Version that computed this row, so a reader can tell an older scoring
    -- version from the current one. A reparse rebuilds every row to the running version,
    -- and the UI gates parsed views during a reparse, so mixed versions are never read
    -- mid-rebuild.
    signals_version        INT NOT NULL,

    -- Outcome and tool health.
    outcome                TEXT NOT NULL,
    outcome_confidence     TEXT NOT NULL,
    score                  INT,
    grade                  TEXT,
    tool_calls             INT NOT NULL DEFAULT 0,
    tool_failures          INT NOT NULL DEFAULT 0,
    tool_retries           INT NOT NULL DEFAULT 0,
    edit_churn             INT NOT NULL DEFAULT 0,
    longest_failure_streak INT NOT NULL DEFAULT 0,

    -- Prompt hygiene. prompt_count is the classifier's base: the session's human prompts
    -- with non-empty content, the exact set the hygiene counts are drawn from. The cohort
    -- aggregate uses it as the rate denominator so a numerator can never exceed it, even
    -- for an agent (Codex, Pi) whose user turns may carry an empty-text, image-only body
    -- that user_message_count would count but the classifier never saw.
    prompt_count           INT     NOT NULL DEFAULT 0,
    short_prompt_count     INT     NOT NULL DEFAULT 0,
    duplicate_prompt_count INT     NOT NULL DEFAULT 0,
    no_code_context_count  INT     NOT NULL DEFAULT 0,
    unstructured_start     BOOLEAN NOT NULL DEFAULT false,

    -- Context health. The two figures are always measured together (see the CHECK below).
    peak_context_tokens    BIGINT,
    context_reset_count    INT,

    refreshed_at           TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT session_signals_version_ck    CHECK (signals_version > 0),
    CONSTRAINT session_signals_outcome_ck    CHECK (outcome IN ('completed', 'abandoned', 'errored', 'unknown')),
    CONSTRAINT session_signals_confidence_ck CHECK (outcome_confidence IN ('high', 'medium', 'low')),
    CONSTRAINT session_signals_score_ck      CHECK (score IS NULL OR (score BETWEEN 0 AND 100)),
    CONSTRAINT session_signals_grade_ck      CHECK (grade IS NULL OR grade IN ('A', 'B', 'C', 'D', 'F')),
    CONSTRAINT session_signals_counts_ck     CHECK (tool_calls >= 0 AND tool_failures >= 0 AND tool_retries >= 0 AND edit_churn >= 0 AND longest_failure_streak >= 0),
    -- Each hygiene count is over a subset of the prompts, so none can exceed the base.
    -- The deriving code preserves that by construction; the CHECK makes the database
    -- enforce it too, so a future classifier bug fails loudly on write rather than
    -- quietly skewing a rate.
    CONSTRAINT session_signals_hygiene_ck CHECK (
        prompt_count >= 0
        AND short_prompt_count BETWEEN 0 AND prompt_count
        AND duplicate_prompt_count BETWEEN 0 AND prompt_count
        AND no_code_context_count BETWEEN 0 AND prompt_count
    ),
    -- Context health is paired: gatherContextHealth either finds no usage and leaves
    -- both NULL, or computes both from the same turn sequence. Enforcing the pairing (both
    -- present or both absent) alongside non-negativity keeps a half-populated row (a
    -- measured peak with a NULL reset the aggregate would read as zero) from existing even
    -- if written by hand.
    CONSTRAINT session_signals_context_health_ck CHECK (
        (peak_context_tokens IS NULL) = (context_reset_count IS NULL)
        AND (peak_context_tokens IS NULL OR peak_context_tokens >= 0)
        AND (context_reset_count IS NULL OR context_reset_count >= 0)
    )
);

-- Aggregate distributions scan by grade and by outcome; both indexes lead with the
-- signals_version so a version-filtered distribution is index-served. The grade index is
-- partial: an unscored session (grade NULL) sits in no grade bucket.
CREATE INDEX idx_session_signals_grade   ON session_signals (signals_version, grade) WHERE grade IS NOT NULL;
CREATE INDEX idx_session_signals_outcome ON session_signals (signals_version, outcome);

-- The Insights page bounds every distribution to a trailing window with
--   WHERE s.started_at >= $since
-- across the grade, outcome, and archetype splits (see analytics_quality.go and
-- analytics_archetype.go). Without a started_at index that lower bound has nothing to seek
-- on, so each bounded request seq-scans the whole sessions table and the work grows with
-- total history rather than with the selected window (the same class of cost migration
-- 0012 fixed for usage_events.occurred_at). This partial index seeks straight to the
-- window's lower bound and skips sessions with no parsed start (started_at NULL), which
-- never fall inside a window. IF NOT EXISTS keeps it replayable on a schema-only dev dump
-- that already carries the index but does not record the migration.
CREATE INDEX IF NOT EXISTS idx_sessions_started_at
  ON sessions(started_at)
  WHERE started_at IS NOT NULL;

-- Link every already-ingested Claude subagent session to the session that spawned it. A
-- subagent runs in its own transcript file that the client nests under the parent's source
-- id ("<parent>/subagents/..."), and akari ingests it as its own session. The schema has
-- modeled the parent link since 0001 (parent_session_id, relationship_type), and the ingest
-- path now records it on announce; this one-time backfill adopts the rows stored before
-- that, matching each child's source-id prefix (the part before "/subagents/") to a parent
-- session of the same user. New sessions link on announce, so this never needs to run again.
UPDATE sessions AS child
   SET parent_session_id = parent.id,
       relationship_type = 'subagent'
  FROM sessions AS parent
 WHERE child.agent = 'claude'
   AND child.parent_session_id IS NULL
   AND position('/subagents/' IN child.source_session_id) > 1
   AND parent.user_id = child.user_id
   AND parent.agent = 'claude'
   AND parent.source_session_id = split_part(child.source_session_id, '/subagents/', 1);

-- Support the adopt-children lookup that runs on every top-level Claude announce. It matches
-- children by the parent-source expression split_part(source_session_id, '/subagents/', 1),
-- so the index is on that expression: equality on it stays index-served under pgx's cached
-- generic plans, where a parameterized LIKE prefix would not. The partial predicate keeps the
-- index to the rows an adopt can still touch (unlinked Claude sessions), and a subagent drops
-- out of it the moment it links.
CREATE INDEX idx_sessions_unlinked_subagents
    ON sessions (user_id, split_part(source_session_id, '/subagents/', 1))
    WHERE agent = 'claude' AND parent_session_id IS NULL;
