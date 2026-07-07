-- Insights rollup tables: per-session pre-aggregations of the projection, written by
-- rebuildTx in the same transaction as the projection rows they summarize (see
-- internal/server/store/rollups.go for the derivations and the grain rule). They key on
-- session_id and carry no dimension sessions or session_signals already carry: project,
-- user, agent, machine, grade, and outcome join in at read time, so grading never touches
-- a rollup row and the filter space stays on the session join.
--
-- The backfill at the bottom populates the corpus from the existing projection rows, so
-- the insights reads are complete the moment this migration lands, with no gap while the
-- epoch-bump reparse drains. Each backfill statement is the corpus-wide form of the
-- per-session derivation in rollups.go; TestRollupBackfillMatchesDerivations pins the two
-- equal, so they cannot drift silently.

-- Usage per (UTC day, model): the four token classes, summed cost, and whether any folded
-- event was token-bearing but unpriced (the read side's cost_incomplete base). day is NULL
-- for undated usage (occurred_at IS NULL), preserving the documented rollup-versus-
-- analytics gap: dated consumers filter day IS NOT NULL.
CREATE TABLE session_usage_daily (
    session_id         BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    day                DATE,
    model              TEXT NOT NULL,
    input_tokens       BIGINT NOT NULL,
    output_tokens      BIGINT NOT NULL,
    cache_read_tokens  BIGINT NOT NULL,
    cache_write_tokens BIGINT NOT NULL,
    cost_usd           DOUBLE PRECISION NOT NULL,
    unpriced           BOOLEAN NOT NULL
);
CREATE INDEX idx_session_usage_daily_session ON session_usage_daily (session_id);
CREATE INDEX idx_session_usage_daily_day ON session_usage_daily (day) WHERE day IS NOT NULL;

-- Tool calls per (tool, category), deduplicated once at write time with the partition the
-- cohort queries used to run per read (dedupToolCallsPartition in analytics_tools.go).
CREATE TABLE session_tool_rollup (
    session_id BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    tool_name  TEXT NOT NULL,
    category   TEXT NOT NULL,
    calls      INT NOT NULL,
    failures   INT NOT NULL,
    PRIMARY KEY (session_id, tool_name, category)
);

-- Deduplicated edit-category calls per churn path (file_rel_path coalesced onto
-- file_path). The "edited more than once" hotness cut stays at read time: it is a window
-- property across sessions, not a per-session fact.
CREATE TABLE session_file_churn (
    session_id BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    churn_path TEXT NOT NULL,
    edits      INT NOT NULL,
    PRIMARY KEY (session_id, churn_path)
);

-- One row per measured prompt-to-reply cycle: the turn ordinal (1 = the session's opening
-- turn, whose latency pays the context load), the prompt instant, and the reply latency in
-- float seconds (extract(epoch ...) verbatim, so percentiles interpolate unchanged).
CREATE TABLE session_turns (
    session_id    BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    turn          INT NOT NULL,
    prompt_at     TIMESTAMPTZ NOT NULL,
    response_secs DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (session_id, turn)
);

-- Timestamped activity per UTC (day, hour): message count, tool-call count (under the
-- owning message's instant), and gap-filtered active seconds (each inter-message gap in
-- (0, 300s] attributed to the later message's hour).
CREATE TABLE session_activity_hourly (
    session_id     BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    day            DATE NOT NULL,
    hour           SMALLINT NOT NULL,
    messages       INT NOT NULL,
    tool_calls     INT NOT NULL,
    active_seconds DOUBLE PRECISION NOT NULL,
    PRIMARY KEY (session_id, day, hour)
);

-- Every project-scoped insights read windows sessions by started_at with a project filter;
-- the bare started_at index made the planner combine or fall back on narrow scopes.
CREATE INDEX idx_sessions_project_started ON sessions (project_id, started_at)
    WHERE started_at IS NOT NULL;

-- backfill (the corpus-wide form of the rollups.go derivations; pinned equal to the
-- per-session form by TestRollupBackfillMatchesDerivations, which executes everything
-- after this marker line against a hand-seeded projection)

INSERT INTO session_usage_daily
  (session_id, day, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd, unpriced)
SELECT ue.session_id,
       (ue.occurred_at AT TIME ZONE 'UTC')::date,
       ue.model,
       coalesce(sum(ue.input_tokens), 0),
       coalesce(sum(ue.output_tokens), 0),
       coalesce(sum(ue.cache_read_tokens), 0),
       coalesce(sum(ue.cache_write_tokens), 0),
       coalesce(sum(ue.cost_usd), 0),
       coalesce(bool_or(ue.cost_usd IS NULL AND (ue.input_tokens + ue.output_tokens + ue.cache_read_tokens + ue.cache_write_tokens + ue.reasoning_tokens) > 0), false)
  FROM usage_events ue
 GROUP BY 1, 2, 3;

INSERT INTO session_tool_rollup (session_id, tool_name, category, calls, failures)
SELECT session_id, tool_name, cat,
       count(*),
       count(*) FILTER (WHERE result_status = 'error')
  FROM (
    SELECT session_id, tool_name,
           coalesce(NULLIF(category, ''), 'other') AS cat,
           result_status,
           row_number() OVER (
             PARTITION BY session_id, call_uid,
               CASE WHEN call_uid IS NULL THEN message_ordinal::text || ':' || call_index END,
               tool_name, coalesce(input_sha256, ''), coalesce(result_status, '')
             ORDER BY message_ordinal, call_index
           ) AS rn
      FROM tool_calls
  ) ranked
 WHERE rn = 1
 GROUP BY session_id, tool_name, cat;

INSERT INTO session_file_churn (session_id, churn_path, edits)
SELECT session_id, churn_path, count(*)
  FROM (
    SELECT session_id,
           COALESCE(file_rel_path, file_path) AS churn_path,
           row_number() OVER (
             PARTITION BY session_id, call_uid,
               CASE WHEN call_uid IS NULL THEN message_ordinal::text || ':' || call_index END,
               tool_name, coalesce(input_sha256, ''), coalesce(result_status, '')
             ORDER BY message_ordinal, call_index
           ) AS rn
      FROM tool_calls
     WHERE category = 'edit' AND file_path IS NOT NULL
  ) ranked
 WHERE rn = 1
 GROUP BY session_id, churn_path;

INSERT INTO session_turns (session_id, turn, prompt_at, response_secs)
WITH m AS (
  SELECT session_id, ordinal, role, timestamp,
         count(*) FILTER (WHERE role = 'user')
           OVER (PARTITION BY session_id ORDER BY ordinal
                 ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS turn
    FROM messages
),
user_turns AS (
  SELECT session_id, turn, min(timestamp) FILTER (WHERE role = 'user') AS user_at
    FROM m WHERE turn >= 1 GROUP BY session_id, turn
),
asst_turns AS (
  SELECT DISTINCT ON (session_id, turn) session_id, turn, timestamp AS asst_at
    FROM m WHERE turn >= 1 AND role = 'assistant' AND timestamp IS NOT NULL
   ORDER BY session_id, turn, ordinal
)
SELECT u.session_id, u.turn, u.user_at, extract(epoch FROM (a.asst_at - u.user_at))
  FROM user_turns u
  JOIN asst_turns a ON a.session_id = u.session_id AND a.turn = u.turn
 WHERE u.user_at IS NOT NULL AND a.asst_at >= u.user_at;

INSERT INTO session_activity_hourly (session_id, day, hour, messages, tool_calls, active_seconds)
WITH msgs AS (
  SELECT session_id,
         (timestamp AT TIME ZONE 'UTC')::date AS day,
         extract(hour FROM timestamp AT TIME ZONE 'UTC')::smallint AS hour,
         extract(epoch FROM (timestamp - lag(timestamp)
           OVER (PARTITION BY session_id ORDER BY ordinal))) AS gap
    FROM messages
   WHERE timestamp IS NOT NULL
),
mh AS (
  SELECT session_id, day, hour, count(*) AS messages,
         coalesce(sum(gap) FILTER (WHERE gap > 0 AND gap <= 300), 0) AS active_seconds
    FROM msgs GROUP BY session_id, day, hour
),
th AS (
  SELECT tc.session_id,
         (m.timestamp AT TIME ZONE 'UTC')::date AS day,
         extract(hour FROM m.timestamp AT TIME ZONE 'UTC')::smallint AS hour,
         count(*) AS tool_calls
    FROM tool_calls tc
    JOIN messages m ON m.session_id = tc.session_id AND m.ordinal = tc.message_ordinal
   WHERE m.timestamp IS NOT NULL
   GROUP BY 1, 2, 3
)
SELECT mh.session_id, mh.day, mh.hour, mh.messages, coalesce(th.tool_calls, 0), mh.active_seconds
  FROM mh
  LEFT JOIN th ON th.session_id = mh.session_id AND th.day = mh.day AND th.hour = mh.hour;
