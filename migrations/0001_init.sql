-- akari initial schema. See docs/DESIGN.md for rationale.
-- Forward-only; applied once and recorded in schema_migrations.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- Identity -----------------------------------------------------------------

CREATE TABLE users (
  id            BIGSERIAL PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,            -- argon2id PHC string (per-user salt + params)
  is_admin      BOOLEAN NOT NULL DEFAULT FALSE,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE api_tokens (
  id           BIGSERIAL PRIMARY KEY,
  user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  scope        TEXT NOT NULL DEFAULT 'ingest' CHECK (scope IN ('ingest', 'full')),
  token_hash   TEXT NOT NULL UNIQUE,            -- sha256 of the presented token
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_used_at TIMESTAMPTZ,
  revoked_at   TIMESTAMPTZ
);
CREATE INDEX idx_api_tokens_user ON api_tokens(user_id);

CREATE TABLE web_sessions (
  id         TEXT PRIMARY KEY,           -- sha256 of the cookie value (never the raw cookie)
  user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX idx_web_sessions_user ON web_sessions(user_id);

CREATE TABLE invite_tokens (
  id          BIGSERIAL PRIMARY KEY,
  token_hash  TEXT NOT NULL UNIQUE,      -- sha256 of the presented invite
  created_by  BIGINT NOT NULL REFERENCES users(id),
  note        TEXT NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at  TIMESTAMPTZ,
  redeemed_by BIGINT REFERENCES users(id),
  redeemed_at TIMESTAMPTZ
);

-- Projects, keyed by canonical git remote -----------------------------------

CREATE TABLE projects (
  id           BIGSERIAL PRIMARY KEY,
  remote_key   TEXT NOT NULL UNIQUE,      -- e.g. github.com/jssblck/akari
  host         TEXT NOT NULL,
  owner        TEXT NOT NULL,
  repo         TEXT NOT NULL,
  display_name TEXT NOT NULL,
  first_seen   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Sessions ------------------------------------------------------------------

CREATE TYPE visibility AS ENUM ('internal', 'public');

CREATE TABLE sessions (
  id                BIGSERIAL PRIMARY KEY,
  user_id           BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  project_id        BIGINT NOT NULL REFERENCES projects(id),
  agent             TEXT NOT NULL CHECK (agent IN ('claude', 'codex', 'pi')),
  source_session_id TEXT NOT NULL,
  parent_session_id BIGINT REFERENCES sessions(id) ON DELETE SET NULL,
  relationship_type TEXT NOT NULL DEFAULT ''
                    CHECK (relationship_type IN ('', 'subagent', 'continuation')),
  machine           TEXT NOT NULL,
  cwd               TEXT NOT NULL DEFAULT '',
  git_branch        TEXT NOT NULL DEFAULT '',
  visibility        visibility NOT NULL DEFAULT 'internal',
  public_id         TEXT UNIQUE,          -- unguessable; set on publish, null otherwise
  started_at        TIMESTAMPTZ,
  ended_at          TIMESTAMPTZ,
  message_count        INT NOT NULL DEFAULT 0,
  user_message_count   INT NOT NULL DEFAULT 0,
  total_input_tokens   BIGINT NOT NULL DEFAULT 0,
  total_output_tokens  BIGINT NOT NULL DEFAULT 0,
  total_cache_write_tokens BIGINT NOT NULL DEFAULT 0,
  total_cache_read_tokens  BIGINT NOT NULL DEFAULT 0,
  total_cost_usd       DOUBLE PRECISION NOT NULL DEFAULT 0,  -- partial sum
  cost_incomplete      BOOLEAN NOT NULL DEFAULT FALSE,        -- any unpriced model
  parser_version    INT NOT NULL DEFAULT 0,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (user_id, agent, source_session_id)
);
CREATE INDEX idx_sessions_project ON sessions(project_id);
CREATE INDEX idx_sessions_user    ON sessions(user_id);
CREATE INDEX idx_sessions_public  ON sessions(id) WHERE visibility = 'public';
CREATE INDEX idx_sessions_parent  ON sessions(parent_session_id)
  WHERE parent_session_id IS NOT NULL;

-- Raw bytes: lossless backup and re-parse source. BYTEA (not TEXT) so arbitrary
-- bytes round-trip exactly and the stored hash matches the client's hash of the
-- raw file. TOAST handles large values.
CREATE TABLE session_raw (
  session_id     BIGINT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
  content        BYTEA NOT NULL DEFAULT '\x',
  byte_len       BIGINT NOT NULL DEFAULT 0,   -- == sessions cursor, line-aligned
  content_sha256 CHAR(64) NOT NULL DEFAULT '' -- sha256 of content; the prefix hash
);

-- Parsed projection ---------------------------------------------------------

CREATE TABLE messages (
  session_id     BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  ordinal        INT NOT NULL,
  role           TEXT NOT NULL,
  content        TEXT NOT NULL,
  thinking_text  TEXT NOT NULL DEFAULT '',
  model          TEXT NOT NULL DEFAULT '',
  timestamp      TIMESTAMPTZ,
  has_thinking   BOOLEAN NOT NULL DEFAULT FALSE,
  has_tool_use   BOOLEAN NOT NULL DEFAULT FALSE,
  content_length INT NOT NULL DEFAULT 0,
  PRIMARY KEY (session_id, ordinal)
);
CREATE INDEX idx_messages_content_trgm ON messages USING gin (content gin_trgm_ops);

CREATE TABLE blobs (
  sha256     CHAR(64) PRIMARY KEY,
  lo_oid     OID NOT NULL,               -- pg_largeobject id
  byte_len   BIGINT NOT NULL,
  media_type TEXT NOT NULL DEFAULT 'application/octet-stream',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- No refcount column: liveness is computed at sweep time (see docs/DESIGN.md CAS).

CREATE TABLE tool_calls (
  session_id        BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  message_ordinal   INT NOT NULL,
  call_index        INT NOT NULL,
  tool_name         TEXT NOT NULL,
  category          TEXT NOT NULL DEFAULT '',
  file_path         TEXT,                  -- convenience, parsed from input
  input_sha256      CHAR(64) REFERENCES blobs(sha256),
  input_bytes       BIGINT,
  input_media_type  TEXT,
  result_sha256     CHAR(64) REFERENCES blobs(sha256),
  result_bytes      BIGINT,
  result_media_type TEXT,
  result_status     TEXT,                  -- ok | error | (empty if pending)
  PRIMARY KEY (session_id, message_ordinal, call_index)
);

CREATE TABLE usage_events (
  id                    BIGSERIAL PRIMARY KEY,
  session_id            BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  message_ordinal       INT,
  model                 TEXT NOT NULL,
  input_tokens          INT NOT NULL DEFAULT 0,
  output_tokens         INT NOT NULL DEFAULT 0,
  cache_write_tokens    INT NOT NULL DEFAULT 0,
  cache_read_tokens     INT NOT NULL DEFAULT 0,
  reasoning_tokens      INT NOT NULL DEFAULT 0,
  cost_usd              DOUBLE PRECISION,
  occurred_at           TIMESTAMPTZ,
  dedup_key             TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_usage_dedup ON usage_events(session_id, dedup_key)
  WHERE dedup_key <> '';

CREATE TABLE attachments (
  id              BIGSERIAL PRIMARY KEY,
  session_id      BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  message_ordinal INT,
  sha256          CHAR(64) NOT NULL REFERENCES blobs(sha256),
  filename        TEXT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
