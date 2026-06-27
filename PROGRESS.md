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

## Milestone 2: parsing (not started)

Parsers (Claude, Codex, pi), incremental parse on chunk, usage + cost, reparse.

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
  that grows via many small appends. Revisit in milestone 2 alongside the parser
  (likely an append-only chunk table + streamed/resumable hash state) when the
  incremental-append pattern is concrete. Correct for now, just not optimal.
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
