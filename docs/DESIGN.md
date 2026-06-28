# akari design

akari backs up AI coding agent sessions from many machines to one shared
Postgres-backed server and lets everyone on that server browse them.

It is split into two programs:

- **akari** (the client): a long-running daemon on each developer machine that
  discovers agent sessions, resolves which git project they belong to, and
  pushes their raw bytes to the server. The client does nothing else: it has no
  UI and stores no durable archive of its own.
- **akari-server** (the server): a single Linux process that ingests raw
  session bytes, parses them, stores them in Postgres, computes stats, and
  serves a web UI where every logged-in user can read every session.

This document is the design. It is written to be implemented from scratch; it
does not assume the reader has seen any other tool.

## Goals

- Back up agent sessions (full text, plus binary attachments where present) from
  Claude Code, Codex, and pi.
- Normalize sessions to a project by **git remote**, so the same repository is
  one project regardless of local path, branch, or worktree, and regardless of
  which machine or user it came from.
- Run continuously by default: detect new session activity and push it with low
  latency, with a one-shot catch-up mode as a fallback.
- Present everything through one server. Any authenticated user sees all
  sessions. Users may additionally publish individual sessions for logged-out
  public viewing.
- Be simple to run: `docker compose up` brings up the server and Postgres for
  local development.

## Non-goals

- No organizations, roles, teams, or per-resource permissions. Authorization is
  flat: logged in means you see everything; public means anyone sees that one
  session.
- No local standalone viewer. Clients only push.
- No DuckDB or any second analytics engine. Postgres is the only datastore.
- No attempt to support every agent. Three parsers (Claude, Codex, pi) with room
  to add more later.

## Platforms

- Server: Linux only.
- Client: Linux, macOS, Windows.

## Top-level architecture

```
  developer machine (xN)                         server (Linux)
  ----------------------                         --------------
  ~/.claude/projects/*.jsonl  ┐
  ~/.codex/sessions/*.jsonl   ├─ discover ─┐
  ~/.pi/agent/sessions/*.jsonl┘            │
                                           ▼
                                    resolve git remote
                                    (cwd -> repo -> remote)
                                           │
                                  raw bytes + project meta
                                           │ HTTPS, Bearer token
                                           ▼
                                   ┌──────────────────┐        ┌────────────┐
                                   │   akari-server   │◀──────▶│  Postgres  │
                                   │  ingest + parse  │        │  + large   │
                                   │  + web UI (SSR)  │        │  objects   │
                                   └──────────────────┘        └────────────┘
                                           ▲
                                  browser (templ + HTMX)
```

The client resolves the git remote locally because the git repository only
exists on the client machine. Everything else (parsing, stats, storage,
rendering) happens on the server, so parser logic lives in exactly one place and
can be improved and re-run against stored raw bytes without re-pushing.

## Core concepts and identity

### Users

A user is a real account: a username and a password. The first account created
bootstraps the server and becomes the admin. After that signup is closed: a new
user can register only by presenting an invite token that an admin issued. Admin
status gates issuing invites, and nothing about data visibility.

Each user holds one or more **API tokens**, and each token has a scope:

- `ingest`: may push session bytes, nothing else. This is what a client daemon
  uses. A leaked ingest token cannot read anyone's data.
- `full`: may push and read (the web and read API), the same access the user has
  in a browser.

The browser authenticates with a password and a session cookie, which always
carries `full` access. Among reads there is no per-user difference: every
authenticated reader sees everything.

### Projects, normalized by git remote

A project is identified by a canonical git remote string. This is the central
design choice and the main place akari differs from prior art that keys projects
off the local directory name.

Resolution is two hops, performed on the client:

1. **Session to local folder.** Each session records the working directory it
   ran in (`cwd`). The client reads that field from the session file header.
2. **Folder to git remote.** The client reads the `origin` remote URL of the git
   repository containing that folder, then canonicalizes it.

Either hop can fail, and a failure classifies the session rather than dropping
it (see "Project resolution and classification" below). A session with a usable
remote is a normal remote project. A session whose folder exists but has no
usable remote is standalone; a session whose folder is unknown or gone is
orphaned. Standalone and orphaned sessions are still backed up, keyed by their
local location (machine plus path) rather than a remote, and tagged in the UI so
their state is explicit. A remote session is never stored under a guessed or
path-derived identity. Only sessions with no remote to find fall back to a local
key.

**Why this collapses worktrees for free.** Git worktrees share the main
repository's config (their `.git` file points at a `commondir`, and remotes live
in the shared config). So `git -C <worktree> remote get-url origin` returns the
same URL from a linked worktree as from the primary checkout. Normalizing by
remote therefore maps every worktree of a repo to the same project with no
special worktree handling. The same property makes branch names irrelevant: the
remote does not change per branch.

**Remote selection.** Only the remote named `origin` is used. If a repository
has no `origin`, or `origin` has more than one URL configured (or its URL is
unrecognized), the session is classified standalone rather than guessed: it is
backed up under its local location instead of a remote. This keeps a remote
project's identity unambiguous and identical on every machine, while still
preserving the work that has no clean remote.

**Canonicalization.** Given the `origin` URL, produce a key of the form
`host/path`:

- Accept all common forms: `git@github.com:owner/repo.git`,
  `ssh://git@github.com/owner/repo.git`, `https://github.com/owner/repo.git`,
  `https://user:token@host/owner/repo`, `git://host/owner/repo`.
- Drop the scheme and any userinfo (credentials).
- Resolve SSH host aliases best effort: if the host matches a `Host` entry in
  the user's `~/.ssh/config` with a `HostName`, substitute the real hostname (so
  `git@gh:owner/repo` becomes `github.com/owner/repo`). This is a best-effort
  step; when it cannot be resolved confidently the alias is left as-is, which at
  worst produces a duplicate project entry rather than a wrong merge.
- Lowercase the host. Drop a default port (22 for ssh, 443 for https).
- Strip a trailing `.git` and any leading slash from the path.
- Lowercase the path only for hosts known to be case insensitive (a built-in set:
  `github.com`, `gitlab.com`, `bitbucket.org`); preserve path case for all other
  hosts.
- Result example: all of the above collapse to `github.com/owner/repo`.

A project row stores the canonical key (unique), plus parsed host, owner, repo,
and a display name (the repo segment), and first/last seen timestamps. It also
records a **kind**: `remote` for a git-remote project, or `standalone` /
`orphaned` for a local folder with no usable remote. A local project's key is
synthetic (`local:<machine>:<cwd>`), so every standalone or orphaned session
from the same folder on the same machine groups into one project. Standalone and
orphaned share that key namespace, so a folder that is later deleted transitions
from standalone to orphaned in place (its kind flips) rather than forking a
second project.

### Sessions

A session is one agent run, identified on the client by its source id (the
session file's UUID or filename stem) and its agent. On the server the natural
key is `(user_id, agent, source_session_id)`; a surrogate id is the primary key.
A session always belongs to exactly one user (the one who pushed it) and exactly
one project: a remote project when resolution succeeds, or a local (standalone or
orphaned) project keyed by machine and path when there is no remote to resolve.
Remote attribution is sticky: once a session resolves to a remote, a later
announce that can no longer find one (its folder was deleted) keeps it under that
remote rather than sliding it into an orphaned bucket. A given session file lives
on one machine and is pushed by one client, so there is never write contention
on a single session from multiple clients.

Sessions can relate to one another. A session records an optional
`parent_session_id` and a `relationship_type`: `subagent` for an agent run
spawned by another (for example Claude's runs under `subagents/`), or
`continuation` for a session forked or resumed from another. The parent and
child are still independent session rows with their own messages and stats; the
link lets the UI nest a subagent under the parent it ran for. The parent is
resolved by source id within the same user and agent, so a subagent that arrives
before its parent is linked once the parent lands.

Cross-user duplication is expected and kept: if two people run agents in the same
repo, those are two sessions (different `user_id`), both visible to everyone and
grouped under the same project. There is no dedup across users.

Visibility is a single enum:

- `internal` (default): visible to any authenticated user.
- `public`: visible to anyone, including logged out.

Publishing a session mints an unguessable `public_id` and serves it at
`/s/{public_id}`; unpublishing clears the id, so the old link stops working
rather than just flipping a flag. There is no "private to me" state, by design:
flat authz means all authenticated users already see all internal sessions.

### Messages, tool calls, usage

The parsed projection of a session:

- **messages**: ordered turns (role, conversational text content, thinking text,
  timestamp, model, flags). The conversational text stays inline so it is
  searchable and rendered directly.
- **tool_calls**: tool invocations attached to a message (name, category, file
  path, and metadata about the input and result). The bulky parts, the tool
  input body and the tool result body, are not stored inline: each is written to
  the content-addressed store and referenced by hash, with its size and media
  type kept on the row. The UI shows them as metadata first (for example
  "36 KB json") and fetches the body from the CAS only when the user expands it.
- **usage_events**: token accounting rows (input, output, cache-creation a.k.a.
  cache-write, cache-read, reasoning) with computed cost, keyed for dedup.

These power reading and stats. They are derived data: the server can drop and
rebuild them from the stored raw bytes whenever the parser improves.

## Server

### Responsibilities

1. Ingest raw session bytes over HTTP (resumable, idempotent).
2. Store raw bytes permanently as the lossless backup and re-parse source.
3. Parse raw bytes into the queryable projection (messages, tool calls, usage).
4. Compute token stats and cost.
5. Extract binary attachments and bulky tool input/result bodies into the
   content-addressed large-object store.
6. Serve a server-rendered web UI and a small read API.
7. Authenticate users and tokens; enforce the internal/public boundary.

### Ingest protocol

All ingest endpoints require `Authorization: Bearer <token>`. The unit of upload
is the raw session file, streamed incrementally by byte offset. The cursor is the
number of bytes the server has stored (`stored_bytes`), and these files only ever
grow by appending, so the protocol is built around append-only growth with an
explicit divergence check.

1. **Announce / upsert session.**
   `POST /api/v1/ingest/session`
   ```json
   {
     "agent": "claude",
     "source_session_id": "0e3b...uuid",
     "project_remote": "github.com/jssblck/akari",
     "git_branch": "main",
     "cwd": "/home/grace/projects/akari",
     "machine": "grace-laptop"
   }
   ```
   The server upserts the project and session rows (latest announce wins for
   mutable metadata like `git_branch` and `cwd`) and replies with the session id,
   the number of raw bytes it holds, and the sha256 of those bytes:
   ```json
   {
     "session_id": 42,
     "stored_bytes": 40960,
     "prefix_sha256": "9f86d0...e7"
   }
   ```
   `prefix_sha256` is the hash of the stored content, the bytes `[0,
   stored_bytes)`. The client hashes the same range of its local file and
   compares. If they match it appends from `stored_bytes`; if they differ (the
   file was rewritten, rotated, or otherwise diverged), or its local file is
   shorter than `stored_bytes`, it resets first. This content check replaces
   fragile signals like inode and device numbers, which are unreliable across
   platforms and rewrites.

2. **Append bytes.**
   `POST /api/v1/ingest/session/{id}/chunk?offset=40960`
   with the raw bytes from that offset as the body. Chunks are always terminated
   on a newline: the client uploads only through the last `\n` it sees, so a
   chunk never includes a partially written final line, and `stored_bytes` always
   rests on a JSONL line boundary. The server:
   - Rejects with the current length if `offset` does not equal `stored_bytes`
     (idempotent: a re-sent chunk whose offset is behind is a no-op that returns
     the truth, so the client simply advances).
   - Rejects a chunk that is empty or does not end on a newline, so the line
     boundary the parser relies on is a server-enforced invariant, not just a
     client convention.
   - Appends the chunk as a new raw row and advances the content hash by resuming
     its stored digest state over only the new bytes. Both are one transaction:
     once it commits, the upload has succeeded regardless of what parsing does.
   - In a second transaction, parses only the bytes past the parse cursor
     (assigning ordinals after the last stored ordinal) and applies them to the
     projection incrementally. Because every stored byte ends on a line boundary,
     the parser only ever sees complete lines. Parsing is best effort: a parse
     failure leaves the durable bytes in place and the cursor where it was, for
     the next chunk or a reparse to advance, so client ingest health never
     depends on parser correctness.
   - Returns the new `stored_bytes` and the new message count.

3. **Reset.** `POST /api/v1/ingest/session/{id}/reset` truncates the raw store
   (its chunks, length, and hash), drops the derived rows, rewinds the parse
   cursor, and re-parses from zero on the next chunk. The client calls this when
   the announce divergence check fails.

Because the server stores raw bytes and `stored_bytes` is the cursor, there is
no separate client-visible sync watermark to keep coherent: the server is always
the source of truth for "how much of this file do you have, and does it still
match mine."

### Server-side parsing pipeline

The parser is a per-agent line reducer: given a small carry-over state (the next
ordinal, and for Codex the sticky model and the open assistant turn) and a region
of complete lines, it returns the next state and a projection delta (rows to add,
results to back-patch, and the increments to fold into the session rollups). The
carry-over is bounded: no per-message or per-tool-call accumulation lives in it,
because the projection rows are themselves the accumulator (a tool result is
back-patched to its call by id, and a Codex turn that spans a chunk boundary keeps
growing the same `messages` row). The state and the parse cursor are stored on the
`session_raw` row, so a chunk parses only its own bytes.

The batch parser used by tests and the bulk reparse is a thin wrapper over the
same reducer fed the whole file at once, so incremental and full parsing cannot
diverge. Reparse (`akari-server reparse [--agent claude]`) reaches already-ingested
data after a parser upgrade: it clears the derived rows, rewinds the cursor, and
replays the stored raw through the reducer from scratch. A session partially
parsed by an older parser version refuses to advance incrementally (the stored
`parse_state_version` no longer matches) until that reparse rewinds it, so a
version change can never blend two parsers' output.

Per-agent specifics the parser must handle:

- **Claude Code** (`~/.claude/projects/<slug>/<id>.jsonl`): newline-delimited
  JSON; messages carry `uuid`/`parentUuid` (a DAG, with forks), `cwd`,
  `gitBranch`, `message.content`, and per-message token usage with
  `input_tokens`, `output_tokens`, `cache_read_input_tokens`,
  `cache_creation_input_tokens`. Subagent runs live under `subagents/`.
- **Codex** (`~/.codex/sessions/YYYY/MM/DD/rollout-*-<uuid>.jsonl` and archived
  flat files): events wrap payloads; `session_meta` carries `cwd` and
  `git.branch`; token totals arrive in `token_count` events as
  `last_token_usage` with a combined input that must be split into uncached
  input and `cache_read` (cached) before storing.
- **pi** (`~/.pi/agent/sessions/<encoded-cwd>/<id>.jsonl`): first line is a
  `type: "session"` header carrying `cwd`; subsequent lines are messages.

Token usage is normalized across agents into one shape: input, output,
cache-write (cache creation), cache-read, reasoning.

### Cost

Cost is computed server-side at parse time from a pricing table compiled into the
binary: a map of model glob to per-million-token rates for input, output,
cache-write, and cache-read. There is no runtime catalog or refresh endpoint;
updating prices means a new build. The computed cost is stored on each usage
event.

A turn whose model is not in the table records its token usage with no cost. The
session's `total_cost_usd` is then the partial sum of the turns that did have
prices, and `cost_incomplete` is set so the UI can show, for example,
"$1.42 (partial)". This keeps a real number visible instead of collapsing the
whole session to unknown when a single turn used an unpriced model.

### Postgres schema

```sql
-- Identity
CREATE TABLE users (
  id            BIGSERIAL PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,            -- argon2id PHC string: embeds a
                                          -- per-user random salt + cost params
  is_admin      BOOLEAN NOT NULL DEFAULT FALSE,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE api_tokens (
  id           BIGSERIAL PRIMARY KEY,
  user_id      BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT NOT NULL,
  scope        TEXT NOT NULL DEFAULT 'ingest',  -- ingest | full
  token_hash   TEXT NOT NULL UNIQUE,      -- sha256 of the presented token
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_used_at TIMESTAMPTZ,
  revoked_at   TIMESTAMPTZ
);

CREATE TABLE web_sessions (              -- browser login cookies
  id         TEXT PRIMARY KEY,           -- random, set in cookie
  user_id    BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL
);

CREATE TABLE invite_tokens (             -- admin-issued, single-use registration
  id          BIGSERIAL PRIMARY KEY,
  token_hash  TEXT NOT NULL UNIQUE,      -- sha256 of the presented invite
  created_by  BIGINT NOT NULL REFERENCES users(id),
  note        TEXT NOT NULL DEFAULT '',
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at  TIMESTAMPTZ,
  redeemed_by BIGINT REFERENCES users(id),
  redeemed_at TIMESTAMPTZ
);

-- Projects, keyed by canonical git remote (or a synthetic local key when there
-- is no remote). kind distinguishes the two; see "Projects, normalized by git
-- remote".
CREATE TABLE projects (
  id           BIGSERIAL PRIMARY KEY,
  remote_key   TEXT NOT NULL UNIQUE,      -- remote: github.com/jssblck/akari; local: local:<machine>:<cwd>
  host         TEXT NOT NULL,             -- remote: hostname; local: machine
  owner        TEXT NOT NULL,
  repo         TEXT NOT NULL,
  display_name TEXT NOT NULL,
  kind         TEXT NOT NULL DEFAULT 'remote',  -- remote | standalone | orphaned
  first_seen   TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_seen    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Sessions
CREATE TYPE visibility AS ENUM ('internal', 'public');

CREATE TABLE sessions (
  id                BIGSERIAL PRIMARY KEY,
  user_id           BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  project_id        BIGINT NOT NULL REFERENCES projects(id),
  agent             TEXT NOT NULL,        -- claude | codex | pi
  source_session_id TEXT NOT NULL,
  parent_session_id BIGINT REFERENCES sessions(id) ON DELETE SET NULL,
  relationship_type TEXT NOT NULL DEFAULT '',  -- '' | subagent | continuation
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

-- Raw bytes: lossless backup and re-parse source. Append-only. The parent row
-- holds the cursor, the prefix hash and its resumable digest state, and the parse
-- cursor + serialized parser state; the bytes themselves are appended as chunk
-- rows so growth is O(append), never a detoast-and-rewrite of the whole value.
CREATE TABLE session_raw (
  session_id          BIGINT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
  byte_len            BIGINT NOT NULL DEFAULT 0,    -- == sessions cursor, line-aligned
  content_sha256      CHAR(64) NOT NULL DEFAULT '...', -- sha256 of all bytes; the prefix hash
  sha256_state        BYTEA,                        -- resumable digest, so hashing is O(append)
  parsed_byte_len     BIGINT NOT NULL DEFAULT 0,    -- how far parsing has consumed
  parse_state_version INT NOT NULL DEFAULT 0,       -- parser version that wrote parse_state
  parse_state         JSONB NOT NULL DEFAULT '{}',  -- bounded per-agent resume cursor
  parse_error         TEXT NOT NULL DEFAULT '',
  CHECK (parsed_byte_len <= byte_len)
);

-- One row per uploaded chunk. The client already trims each chunk to a newline,
-- so every row boundary is a JSONL line boundary and a parse can resume at any of
-- them. byte_offset is the sequence.
CREATE TABLE session_raw_chunks (
  session_id  BIGINT NOT NULL REFERENCES session_raw(session_id) ON DELETE CASCADE,
  byte_offset BIGINT NOT NULL,
  byte_len    BIGINT NOT NULL,
  content     BYTEA NOT NULL,
  PRIMARY KEY (session_id, byte_offset)
);

-- Parsed projection
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
  is_open        BOOLEAN NOT NULL DEFAULT FALSE,  -- turn still accumulating (Codex fold across chunks)
  content_length INT GENERATED ALWAYS AS (octet_length(content)) STORED,
  PRIMARY KEY (session_id, ordinal)
);
-- Trigram index for full-text search over message content
CREATE INDEX idx_messages_content_trgm ON messages USING gin (content gin_trgm_ops);

CREATE TABLE tool_calls (
  session_id        BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  message_ordinal   INT NOT NULL,
  call_index        INT NOT NULL,
  tool_name         TEXT NOT NULL,
  category          TEXT NOT NULL DEFAULT '',
  file_path         TEXT,                  -- convenience, parsed from input
  -- Bulky bodies live in the CAS; the row keeps only references and metadata.
  input_sha256      CHAR(64) REFERENCES blobs(sha256),
  input_bytes       BIGINT,
  input_media_type  TEXT,                  -- e.g. application/json
  result_sha256     CHAR(64) REFERENCES blobs(sha256),
  result_bytes      BIGINT,
  result_media_type TEXT,
  result_status     TEXT,                  -- ok | error | (empty if pending)
  call_uid          TEXT,                  -- agent's call id; back-patches the result by UPDATE
  source_offset     BIGINT,                -- raw byte offset of the originating line
  PRIMARY KEY (session_id, message_ordinal, call_index)
);
-- Non-unique: a duplicate id (which agents do not emit) must never fail an
-- append, since raw bytes are authoritative and the projection is reparse-able.
CREATE INDEX idx_tool_calls_call_uid ON tool_calls(session_id, call_uid)
  WHERE call_uid IS NOT NULL;

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
  dedup_key             TEXT NOT NULL DEFAULT '',
  source_offset         BIGINT,           -- raw byte offset of the originating line
  source_index          INT NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX idx_usage_dedup ON usage_events(session_id, dedup_key)
  WHERE dedup_key <> '';
-- Source identity makes incremental inserts idempotent even for Codex, whose
-- usage carries no native dedup key; a replayed line is absorbed by ON CONFLICT.
CREATE UNIQUE INDEX idx_usage_source ON usage_events(session_id, source_offset, source_index)
  WHERE source_offset IS NOT NULL;

-- Content-addressed store (Postgres large objects): anything too large to inline
-- (binary attachments, bulky tool input/result bodies), deduped by content hash.
CREATE TABLE blobs (
  sha256     CHAR(64) PRIMARY KEY,
  lo_oid     OID NOT NULL,               -- pg_largeobject id
  byte_len   BIGINT NOT NULL,
  media_type TEXT NOT NULL DEFAULT 'application/octet-stream',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- No refcount column: liveness is computed at sweep time (see CAS).

CREATE TABLE attachments (
  id              BIGSERIAL PRIMARY KEY,
  session_id      BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  message_ordinal INT,
  sha256          CHAR(64) NOT NULL REFERENCES blobs(sha256),
  filename        TEXT,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- No pricing table: model rates are compiled into the binary (see Cost).
```

### Content-addressed store (CAS)

The CAS is the home for any content that should not be inlined into a table row:
large objects, whatever their type. It is content-addressed (keyed by sha256) and
deduped across all sessions and users. The dividing line is size and role, not
text versus binary. Today that means two kinds of content: binary attachments
(for example images pasted into a Claude session) and the bulky bodies of tool
calls (tool input and tool result), which are often large JSON or file dumps.
Keeping these out of the hot tables keeps those tables small, dedupes content
that recurs across sessions, and lets the UI defer loading a body until the user
asks for it.

Writing a blob:

- Hash the bytes with sha256.
- Insert the large object and `blobs` row if the hash is new, otherwise do
  nothing: create the Postgres large object (`lo_create` / write via the
  large-object API inside a transaction) and insert the `blobs` row with its
  `media_type` under `ON CONFLICT (sha256) DO NOTHING`. Writes never touch a
  count.
- Point at the hash from the referencing row: an `attachments` row, or the
  `input_sha256` / `result_sha256` columns on a `tool_calls` row.

Liveness is not tracked with a refcount; it is computed when needed. Deleting a
session simply cascades its referencing rows away, and a re-parse drops and
rewrites them. A sweep then deletes any blob that no referencing row points at
(`lo_unlink` plus row delete, for blobs where `NOT EXISTS` an `attachments` or
`tool_calls` reference). This makes ingest cheaper (no counting) and the scheme
self-healing (a drifted count can never strand or prematurely free a blob). The
sweep only needs to run after deletions or re-parses, since nothing else can
orphan a blob.

Conversational text (message content and thinking) stays inline in `messages`, so
it remains searchable and renders straight from the table, and the raw session
bytes live in their own append-only `session_raw_chunks` table as the lossless
backup and reparse source. The CAS takes the large objects: binary attachments
and bulky tool bodies today, and anything else that would bloat a row later,
shown in the UI as metadata until expanded.

Serving a blob is access-controlled through the session that references it, not
by the bare hash. Because a single blob can be shared by an `internal` and a
`public` session, the fetch is session-scoped: authenticated viewers use
`/api/v1/session/{id}/blob/{sha256}`, and logged-out viewers use
`/s/{public_id}/blob/{sha256}`. In both cases the server checks the viewer may
see that session (authenticated, or reached through a valid `public_id`) and that
the session actually references the hash before streaming it. This prevents the
content-addressed dedup from leaking an internal body through a public session,
and never exposes the numeric session id on the public path.

### Web UI (server-rendered)

The UI is server-rendered Go using `templ` for templates and HTMX for
interactivity (filtering, pagination). In-progress sessions update live over
server-sent events: the session view subscribes to an SSE stream and swaps in new
messages and stats as the server parses incoming bytes. No Node toolchain; the
binary is self-contained.

Pages:

- **Login**, and **registration** (requires a valid invite token).
- **Projects index**: two sections. **Projects** lists every git-remote project
  with session counts, token totals, and last activity. **Sessions** lists local
  folders with no git remote (standalone and orphaned), each tagged with its
  state and labeled by folder name and path rather than the synthetic key. A
  folder with no local sessions shows no Sessions section.
- **Project view**: sessions in that project across all users and machines, with
  filters (user, agent, machine, date, model).
- **Session view**: the transcript (messages, thinking, tool calls,
  attachments), with a stats header (tokens in/out/cache-read/cache-write,
  cost, duration, message counts). Tool inputs and results render as metadata
  chips (for example "36 KB json") that expand on click to fetch the body from
  the CAS. Any subagent sessions are shown nested under the call that spawned
  them. A publish/unpublish control for the owner.
- **Public session view**: the same session view at `/s/{public_id}` (the
  unguessable id minted on publish), served without auth. Unpublishing clears the
  id and the link dies.
- **Search**: trigram search across message content, scoped to a project or
  global, with the same user / agent / date filters available on results.
- **Account**: manage API tokens (create with a scope, name, revoke); admins
  also issue and revoke invite tokens here.

Read endpoints backing HTMX fragments live under `/api/v1/...` and return HTML
partials, not JSON, to keep the rendering in one place.

### Auth specifics

- Passwords hashed with argon2id. Each password gets a fresh cryptographically
  random salt at set time; the salt and the cost parameters are stored inside the
  PHC-encoded `password_hash` string (no separate plaintext or shared salt), so
  two users with the same password produce different hashes.
- Browser sessions: opaque cookie id backed by `web_sessions`, rotated on login,
  cleared on logout.
- API tokens: a long random string shown once at creation; only its sha256 is
  stored. Presented as `Bearer`. `last_used_at` updated on use. Each token is
  scoped `ingest` (push only) or `full` (push and read).
- Registration requires a valid, unredeemed invite token (stored as a sha256
  hash, single use). The first-ever account bootstraps without one and becomes
  admin; every later account must redeem an invite an admin issued.
- Authorization checks, and only these: ingest endpoints require any valid token
  (scope `ingest` or `full`); reads require a browser session or a `full` token,
  or that the specific session is public (for logged-out reads). Owner-only
  actions are limited to publish/unpublish and token management.

## Client

The client is a thin, long-running pusher that keeps no durable state on disk
(beyond its config file). The server is authoritative for how much of each file
has been stored, so the client never has to remember anything across runs: it
discovers files, asks the server where each one stands, and uploads the gap.
Client CPU is cheap, so the client recovers by re-announcing rather than by
persisting any state of its own.

### Discovery

Enumerate session files for each agent from its known roots. Each agent's own
documented override is honored when present; akari defines no environment
variables of its own (see Config):

- Claude: `~/.claude/projects/**/*.jsonl` (and `subagents/`), `CLAUDE_PROJECTS_DIR`.
- Codex: `~/.codex/sessions/**/rollout-*.jsonl` and archived sessions,
  `CODEX_SESSIONS_DIR`.
- pi: `~/.pi/agent/sessions/*/*.jsonl` (validated by a `type: "session"`
  header), `PI_DIR`.

Extra or non-standard roots are added through the config file, not through new
environment variables.

### Project resolution and classification

For each discovered file, the client peeks the header: it reads from the top only
as far as it needs to extract `cwd`, the source session id, and the agent. That is
cheap and usually the first few lines, though for Codex the `cwd` arrives in an
early `session_meta` event, so the peek reads until it finds it. The full parse
is the server's job. With the header in hand the client classifies the session
into one of three kinds, and backs up all three:

1. **Orphaned.** If `cwd` is empty or no longer exists on disk, the session is
   orphaned: its location can never be resolved to a remote. The reason is
   recorded (`no working directory recorded` or `cwd no longer exists`).
2. **Standalone.** Otherwise run `git -C <cwd> rev-parse --is-inside-work-tree`,
   then `git -C <cwd> remote get-url --all origin`. Any failure (`<cwd> is not a
   git repository`, `... has no origin remote`, `... origin has multiple URLs`,
   or an unrecognized origin URL) makes the session standalone: a real local
   folder with no clean remote. A repository with remotes but no `origin` is
   treated the same as one with no remote.
3. **Remote.** A single usable `origin` is canonicalized (see Projects); the
   result is the project key sent on ingest.

A remote session uploads with its canonical key. A standalone or orphaned session
uploads with its kind and its working directory; the server derives the synthetic
local key from machine plus path. The per-kind counts are surfaced (a periodic
summary in watch mode, a final tally in one-shot mode) so a user can see what is
backed up as standalone or orphaned. Only a file whose header cannot be read at
all is truly skipped, since there is then nothing to identify or send. Git is
invoked by shelling out to the system `git` with a short timeout; results are
cached per directory for the process lifetime.

### Upload

Drive the ingest protocol above, statelessly, once per file each time it is
visited:

- Announce the session, learn `stored_bytes` and the server's `prefix_sha256`.
- Verify: hash the local file's first `stored_bytes` bytes and compare. On
  mismatch (or a local file shorter than `stored_bytes`), call reset and
  re-upload from zero; otherwise resume at `stored_bytes`.
- Stream the gap in bounded chunks (a few MB), each truncated to the last newline
  so only complete JSONL lines are sent, advancing on each ack.

There is nothing to persist on the client. If the local file already matches the
server (size equals `stored_bytes`, hashes agree), the announce is the only call
and no bytes move. Restarts, crashes, and a fresh machine all recover by simply
re-announcing; divergence is always decided by the server's `prefix_sha256`.

### Watch mode (default)

`akari watch` runs continuously:

- Watch each agent root with `fsnotify` (inotify / FSEvents / ReadDirectory
  changes), recursively, with a budget. New directories under a root are added
  automatically.
- Debounce events (about 500 ms) and coalesce bursts, then upload the changed
  files.
- Fall back to periodic polling (a few seconds) for roots the OS watcher cannot
  cover (resource exhaustion such as too many watches, or network filesystems),
  and a slow full rescan on a long timer (for example every 15 minutes) as a
  safety net.

### One-shot mode

`akari sync` does a single discovery pass, uploads everything new since the
server's `stored_bytes` per file, prints a summary (uploaded, skipped with
reasons), and exits. This is the catch-up / cron-friendly mode.

### Daemon management

`akari watch` is the foreground loop. `akari daemon {start|stop|status}` manages
it as a background process per OS:

- Linux: a systemd user unit (generated and enabled), or a detached process with
  a pidfile when systemd is absent.
- macOS: a launchd LaunchAgent plist.
- Windows: a detached background process (no console window), optionally
  registered with Task Scheduler for start-at-login.

A single advisory file lock ensures only one client instance runs per machine.

### Client config

- All configuration lives in one config file at the platform-standard per-user
  location (via Go's `os.UserConfigDir()`: `~/.config/akari/config.toml` on
  Linux, `~/Library/Application Support/akari/config.toml` on macOS,
  `%AppData%\akari\config.toml` on Windows). It holds the server URL, API token,
  any extra session roots, and watch excludes. akari reads no environment
  variables of its own; the only env it consults are the agents' own documented
  overrides (`CLAUDE_PROJECTS_DIR`, `CODEX_SESSIONS_DIR`, `PI_DIR`) while
  locating their session roots.
- There is no on-disk state. The git resolution cache (directory to remote) is
  kept in memory for the process lifetime only; everything else the client needs
  to know it gets from the server on announce.

## Stats

Computed on the server from `usage_events` and `messages`:

- Per session: total input, output, cache-write, cache-read tokens; cost;
  message and user-message counts; duration.
- Aggregated by project, user, agent, model, and time bucket for the project and
  index views.

Cache-write maps to the providers' cache-creation tokens; cache-read maps to
cache-read tokens.

## Deployment

Local development and the reference deployment both use Docker Compose:

```yaml
# docker-compose.yml (sketch)
services:
  postgres:
    image: postgres:18              # latest major
    environment:
      POSTGRES_DB: akari
      POSTGRES_USER: akari
      POSTGRES_PASSWORD: akari
    volumes: [pgdata:/var/lib/postgresql/data]
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U akari"]
  server:
    build: .
    depends_on:
      postgres: { condition: service_healthy }
    environment:
      AKARI_DATABASE_URL: postgres://akari:akari@postgres:5432/akari?sslmode=disable
      AKARI_LISTEN: ":8080"
    ports: ["8080:8080"]
volumes: { pgdata: {} }
```

The `pg_trgm` extension is enabled by a migration. Schema migrations run on
server startup (embedded SQL, forward-only). For production the server expects
TLS termination in front of it (or a reverse proxy); signup is invite-only after
the first admin account, so an exposed server is not openly registerable.

The "no environment variables of our own" rule is a client policy: the client has
a per-user home and uses a config file there. The server is a container workload
with no such home, so it follows container convention and takes its
configuration (`AKARI_DATABASE_URL`, `AKARI_LISTEN`, and similar) from the
environment.

## Repository layout

The current scaffold (`main.go` plus `internal/greet`) is placeholder and will
be replaced by:

```
cmd/
  akari/            # client binary
  akari-server/     # server binary
internal/
  parser/           # claude, codex, pi parsers + normalized types (server-side)
  gitremote/        # remote URL canonicalization
  pricing/          # compiled-in rate table + cost computation
  server/
    httpapi/        # ingest + read handlers
    web/            # templ templates, HTMX fragments
    store/          # postgres queries, CAS (large objects), migrations
    auth/           # password, tokens, cookies
    parse/          # parse pipeline + reparse
  client/
    discover/       # session file enumeration
    resolve/        # cwd -> git remote, skip-and-warn
    upload/         # ingest protocol driver (stateless)
    watch/          # fsnotify + polling fallback
    daemon/         # per-OS background management
  config/           # shared config loading
migrations/         # forward-only SQL
docker-compose.yml
Dockerfile
```

## Tooling

- Go (current toolchain pinned in `go.mod`).
- `templ` for templates, `htmx` served as a static asset.
- `pgx` for Postgres (large-object support, batching).
- `fsnotify` for file watching. The client keeps no on-disk state.
- Tests: per-agent parser fixtures (recorded raw session files), git
  canonicalization table tests, an ingest protocol test against a Postgres
  container, and resolution skip-and-warn tests.

## Milestones

These are a build order for development, sequenced for implementation ease, not a
staged production rollout. akari is not meant to run in a partial state in
production: the server ships once it is whole, and later milestones are not pushed
to a live deployment incrementally. The order below can be rearranged freely as
long as the final result is complete.

1. **Server foundation**: schema + migrations, auth (first-admin bootstrap,
   invite-only registration, login, API tokens), ingest endpoints, raw storage,
   `docker-compose up` works end to end.
2. **Parsing**: Claude, Codex, pi parsers; incremental parse on chunk; usage and
   cost; `reparse` command.
3. **Client core**: discovery, git remote resolution with skip-and-warn,
   one-shot `sync`.
4. **Client watch + daemon**: fsnotify, polling fallback, per-OS background
   management.
5. **Web UI**: projects, project view, session view with stats, search.
6. **Public publishing**: visibility toggle and logged-out session view.
7. **CAS**: extract binary attachments and bulky tool input/result bodies to
   large objects, render them as expandable metadata in the UI, session-scoped
   blob serving, orphan sweep (no refcounts).
8. **Polish**: docs, broader fixtures, retention/sweep tuning.
```
