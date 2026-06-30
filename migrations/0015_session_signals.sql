-- session_signals holds per-session derived behavioral signals: an outcome
-- classification (completed / abandoned / errored / unknown), a 0-100 quality score
-- and its A-F grade, and the tool-health counts the score is built from.
--
-- It is a derived projection table, rebuilt from a session's own messages and
-- tool_calls whenever the session catches up to its stored bytes (the live parse
-- path) or is reparsed. So it never participates in the token-rollup invariant
-- (sessions.total_* == sum over usage_events), and a stale or missing row self-heals
-- on the next rebuild rather than being a correctness bug. The ON DELETE CASCADE ties
-- the row to its session; the parse/reset paths that clear projection rows without
-- deleting the session delete this row too.
CREATE TABLE session_signals (
    session_id             BIGINT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
    -- The quality.Version that computed this row. Stamped so a reader can tell a row
    -- built by an older scoring version from a current one; a reparse rebuilds every
    -- row to the running version, and the UI gates parsed views during a reparse, so
    -- mixed versions are never read mid-rebuild.
    signals_version        INT NOT NULL,
    outcome                TEXT NOT NULL,
    outcome_confidence     TEXT NOT NULL,
    -- score/grade are NULL for an unscored session: an unknown outcome with no tool
    -- signal carries no meaningful grade, the same restraint agentsview shows rather
    -- than grading an empty automated blip.
    score                  INT,
    grade                  TEXT,
    tool_calls             INT NOT NULL DEFAULT 0,
    tool_failures          INT NOT NULL DEFAULT 0,
    tool_retries           INT NOT NULL DEFAULT 0,
    edit_churn             INT NOT NULL DEFAULT 0,
    longest_failure_streak INT NOT NULL DEFAULT 0,
    refreshed_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT session_signals_version_ck    CHECK (signals_version > 0),
    CONSTRAINT session_signals_outcome_ck    CHECK (outcome IN ('completed', 'abandoned', 'errored', 'unknown')),
    CONSTRAINT session_signals_confidence_ck CHECK (outcome_confidence IN ('high', 'medium', 'low')),
    CONSTRAINT session_signals_score_ck      CHECK (score IS NULL OR (score BETWEEN 0 AND 100)),
    CONSTRAINT session_signals_grade_ck      CHECK (grade IS NULL OR grade IN ('A', 'B', 'C', 'D', 'F')),
    CONSTRAINT session_signals_counts_ck     CHECK (tool_calls >= 0 AND tool_failures >= 0 AND tool_retries >= 0 AND edit_churn >= 0 AND longest_failure_streak >= 0)
);

-- Aggregate distributions scan by grade and by outcome; both indexes lead with the
-- signals_version so a version-filtered distribution is index-served. The grade index
-- is partial: an unscored session (grade NULL) sits in no grade bucket.
CREATE INDEX idx_session_signals_grade   ON session_signals (signals_version, grade) WHERE grade IS NOT NULL;
CREATE INDEX idx_session_signals_outcome ON session_signals (signals_version, outcome);
