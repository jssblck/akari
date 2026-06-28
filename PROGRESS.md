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

## Milestone 3: client core (DONE)

Goal: the thin client end of the pipeline. Discover agent session files, peek
each header for cwd, resolve cwd to a canonical git origin remote (skip-and-warn
on failure), and drive the append-only ingest protocol statelessly. One-shot
`akari sync`.

- [x] internal/gitremote: origin URL canonicalization (scp-like, scheme URLs,
      ports, userinfo, case rules, nested subgroups) + best-effort ssh alias
      resolution
- [x] internal/config: client TOML config at os.UserConfigDir, atomic save,
      raw vs validated load
- [x] internal/client/discover: per-agent roots (env overrides honored), file walk
- [x] internal/client/resolve: header peek per agent, git resolution via injectable
      runner, skip-and-warn with specific reasons, per-directory cache
- [x] internal/client/upload: stateless ingest driver (announce, prefix-hash
      verify, reset on divergence, newline-trimmed chunking with long-line growth,
      409 re-announce-and-reverify)
- [x] cmd/akari: `sync` (one-shot, dry-run, summary with skip reasons) and `login`
- [x] Tests: gitremote table tests, discover, resolve (peek + skip cases + cache),
      upload (resume/reset/uptodate/conflict/long-line), config round-trip
- [x] e2e: built the client, ran `akari sync` against the live server with a real
      git repo as cwd; verified resolution to host/owner/repo, incremental append,
      idempotent re-sync, and skip-and-warn for a non-git cwd
- [x] codex review (gpt-5.5 high) twice; all findings fixed and re-verified

### Milestone 3 codex findings (all fixed)

- HIGH: a mid-stream 409 conflict trusted the server's reported cursor without
  re-checking the prefix hash, so the client could append onto a divergent server
  prefix, and could in principle loop forever. SyncFile now re-announces and
  re-verifies the prefix from scratch on conflict, bounded by a retry cap.
- HIGH: SaveClient truncated the config before encoding, so a failure could
  destroy the only stored token. It now writes a temp file and atomically renames.
- HIGH: `akari login` swallowed all config-load errors, silently overwriting a
  corrupt config and losing extra_roots. It now uses ReadClient, which separates
  missing (start blank) from corrupt (refuse to overwrite).
- HIGH: ssh alias resolution rewrote any matching host, so a Host github.com /
  HostName ssh.github.com entry split a repo across two projects. Only short,
  dotless aliases are resolved now; canonical hosts are left alone.
- LOW: SyncFile discarded the outcome of conflicted attempts, undercounting bytes
  and missing a reset in the summary. Work is now rolled up across retries.

## Milestone 4: client watch + daemon (DONE)

Goal: continuous watch mode and background daemon management. fsnotify with a
polling fallback and a slow full rescan, a single-instance lock, and per-OS
detached process management.

- [x] internal/client/syncer: shared resolve+upload used by both sync and watch
- [x] internal/client/watch: fsnotify event loop with debounce, polling fallback
      (mtime/size diff), periodic full rescan, recursive auto-add of new dirs,
      and an unbounded deduped dirty set drained by a single worker
- [x] internal/client/daemon: OS advisory lock (flock / LockFileEx) for single
      instance, plus Start/Stop/Status of a detached `akari watch` process
- [x] cmd/akari: `watch` (foreground, holds the lock) and `daemon {start|stop|status}`
- [x] Tests: watch (initial pass, new-file detection, non-session filtering),
      daemon lock (acquire/release, double-acquire rejection, IsRunning, alive)
- [x] e2e: started the daemon against the live server with isolated discovery,
      saw the initial pass and a live append uploaded, confirmed status,
      double-start rejection, and clean stop with the lock released
- [x] codex review (gpt-5.5 high) twice; all findings fixed and re-verified

### Milestone 4 codex findings (all fixed)

- HIGH: the watch worker queue was a bounded channel that dropped files when full
  (>1024 files or a slow worker), and the poll baseline was updated as if the drop
  had synced. Replaced with an unbounded, deduplicated dirty set drained by the
  worker, so no change is ever lost.
- HIGH: the pidfile lock had a stale-reclaim TOCTOU race and trusted a recorded
  pid (vulnerable to pid reuse). Replaced with a real OS advisory lock held for
  the process lifetime (auto-released on death); IsRunning probes the live lock.
- HIGH: Release unlinked the pidfile after unlocking, a window in which a new
  holder's file could be deleted. Release now only drops the OS lock and leaves
  the (harmless, unlocked) file in place.
- HIGH: daemon Start polled with IsRunning, competing with the child for the lock
  and risking a false startup timeout. Start now watches for the child to write
  its own pid (and bails if the child exits) without touching the lock.
- MEDIUM: Acquire ignored pidfile write errors, which Stop depends on. A write
  failure is now fatal (unlock, close, error).
- Windows detail: the lock is taken on a high sentinel byte offset so the pid
  bytes stay readable (an exclusive LockFileEx range is otherwise unreadable).

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
