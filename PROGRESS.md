# akari implementation progress

Working tracker across milestones. See DESIGN.md for the full design.

## Conventions

- One git branch per milestone, merged to `main` after codex review + tests.
- Code review via codex (gpt-5.5, high effort) before each milestone commit; do
  not over-index on nitpicks.
- End-to-end test once feasible (docker compose up, exercise endpoints/UI).

## Milestone 1: server foundation (DONE)

Goal: schema + migrations, auth (first-admin bootstrap, invite-only registration,
login, API tokens), ingest endpoints, raw storage, `docker compose up` works end
to end.

- [x] Project restructure: remove greet scaffold, add cmd/akari-server, cmd/akari stub
- [x] Dependencies: pgx/v5, x/crypto (argon2)
- [x] migrations/0001_init.sql (full schema from DESIGN.md)
- [x] internal/config: server config from env
- [x] internal/server/store: pgx pool + migration runner + queries
- [x] internal/server/auth: argon2 passwords, token + invite hashing
- [x] internal/server/httpapi: router, middleware, auth/register/login/logout, token+invite mgmt, ingest endpoints
- [x] cmd/akari-server/main.go wiring
- [x] Dockerfile + docker-compose.yml + .dockerignore
- [x] Tests: unit (auth, parseRemoteKey) + ingest protocol against pg container
- [x] e2e: docker compose up, register/token/announce/chunk/reset/invite/authz (20 checks pass)
- [x] codex review (gpt-5.5 high), fixes applied, deferred items recorded

## Milestone 2: parsing (DONE)

Goal: server-side parsers for all three agents, parse-on-chunk into a normalized
projection (messages, tool calls, usage), compiled-in pricing with a partial-sum
cost and an incomplete flag, and a `reparse` subcommand to rebuild from raw.

- [x] internal/parser: types, dispatch, Claude/Codex/pi parsers (gjson)
- [x] internal/pricing: compiled-in rate table, longest-prefix lookup, partial cost
- [x] internal/server/parse: raw -> parse -> price -> WriteProjection pipeline
- [x] internal/server/store/projection.go: LoadRaw, SessionsForReparse, WriteProjection
- [x] handleChunk parses on every append; returns real message_count
- [x] cmd/akari-server reparse subcommand (--agent filter)
- [x] Tests: parser unit (fixtures per agent), pricing, parse-pipeline integration,
      stale-projection guard; full suite green (run with `-p 1`, shared test DB)
- [x] e2e: register -> ingest token -> announce -> chunk over HTTP, projection +
      cost verified in DB; reparse confirmed idempotent against live data
- [x] codex review (gpt-5.5 high) twice; all findings fixed and re-verified

### Milestone 2 codex findings (all fixed)

- HIGH: a parse running outside the append transaction could overwrite a newer
  projection (live ingest vs reparse, or interleaved chunks). WriteProjection now
  takes the raw byte length it parsed from, locks `session_raw FOR UPDATE`, and
  aborts with ErrStaleProjection if the stored length moved; SessionFromRaw treats
  that as a no-op. Whichever parse matches current raw wins.
- HIGH: scanLines swallowed scanner errors (an over-long line or read failure),
  which could feed a truncated session to WriteProjection. It now returns the
  error; all parsers propagate it, so a bad read keeps the prior projection.
- HIGH: the Codex parser never reset the current assistant turn, so a later turn's
  tool calls and usage could attach to the previous assistant, and an assistant
  text item after reasoning/function_call spawned a duplicate message. A user item
  now resets the turn and final text folds into the turn's single message.
- MEDIUM: tool-result sizes were derived from flattened text. Result size and
  media type now come from the original body (string -> text/plain, array/object
  -> application/json), matching what the CAS will store.
- MEDIUM: empty tool-call IDs were indexed, so a malformed result could match the
  wrong call. Empty IDs are no longer indexed or matched.
- LOW: reparse used flag.ExitOnError; now ContinueOnError so it returns errors.

## Milestone 3: client core (not started)

Discovery, git remote resolution + skip-and-warn, one-shot sync.

## Milestone 4: client watch + daemon (not started)

## Milestone 5: web UI (not started)

## Milestone 6: public publishing (not started)

## Milestone 7: CAS (not started)

## Milestone 8: polish (not started)

## Deferred (from codex review, to address in later milestones)

- Raw storage is a single growing BYTEA column, so each append rewrites the row
  and recomputes the prefix hash over the whole content: quadratic for a file
  that grows via many small appends. Not revisited in milestone 2 (the parser did
  not need to touch raw storage). Revisit when the client's real append cadence is
  known (likely an append-only chunk table + streamed/resumable hash state).
  Correct for now, just not optimal.
- Milestone 2 reparse runs the whole parse pipeline per append. For very large
  sessions appended in many chunks this re-parses the full file each time. Fine at
  current scale; revisit with incremental parsing if it shows up in practice.
- Chunk `AppendChunk` returns the true cursor for any offset that does not equal
  the stored length. A stale offset carrying *different* bytes is not byte-checked
  against stored content; this is currently safe because divergence is caught at
  announce by the prefix-hash check, but add overlap verification or force a
  reset as defense in depth later.

## Notes / decisions made during build

- Local dev: an unrelated postgres runs on host port 54329; akari compose uses a
  different host port to avoid conflict.
- Postgres 18 needs the data mount at `/var/lib/postgresql` (not `.../data`).
- session_raw.content is BYTEA (lossless raw bytes), distinct from the inline
  TEXT used for searchable message content in later milestones.
- web_sessions.id stores sha256(cookie); the raw cookie is never persisted.
- Integration tests share one Postgres database and each resets the public schema,
  so run them serialized: `go test -p 1 ./...`. They skip unless
  AKARI_TEST_DATABASE_URL is set (local: a dedicated `akari_test` database).
- Tool-call input/result bodies are stored as size + media type only in M2; the
  CAS milestone will store the bodies themselves and back-reference them.
