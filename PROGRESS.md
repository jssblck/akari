# akari implementation progress

Working tracker across milestones. See docs/DESIGN.md for the full design.

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
- [x] migrations/0001_init.sql (full schema from docs/DESIGN.md)
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

## Milestone 5: web UI (DONE)

Goal: the server-rendered read UI. Login/register, a projects index with rolled-up
stats, a project view with agent/user/machine filters, a session view (stats
header, transcript with thinking and tool-metadata chips, subagents), full-text
search, account/token/invite management, and live session updates over SSE.

- [x] internal/server/web: templ templates (layout, login/register, projects,
      project + filterable session list, session + body fragment, transcript,
      tool chip, search, account, error), view-model helpers (token/byte/cost/
      time/duration formatting, role and status classes), embedded static assets
- [x] internal/server/web/static: dark theme stylesheet, htmx 2.0.3, and an
      app.js that wires the SSE stream to an htmx body swap via data attributes
- [x] internal/server/store/read.go: ListProjects, Project, ListSessions (dynamic
      filter), SessionDetail by id/public id, Messages, ToolCalls, Subagents,
      Search (trigram ILIKE), SessionFacets (filter dropdown values)
- [x] internal/server/httpapi/web.go: HTML handlers, requireReadHTML middleware,
      flash cookies for once-shown secrets, login/register/logout forms, account
      token/invite forms, SSE + body-fragment endpoints, static serving
- [x] internal/server/httpapi/sse.go: in-process hub (subscribe/unsubscribe/
      publish keyed by session id); handleChunk publishes after each parse
- [x] Tests: full web flow (unauth redirect, register, projects, token flash,
      search, logout), login next-preservation, safeNext open-redirect guard
- [x] e2e: real browser (Claude in Chrome) through login, projects, project view,
      session view, search, account; live SSE verified by streaming a session in
      two chunks and watching the page update message count, stats, tool result,
      and the new message with no reload
- [x] codex review (gpt-5.5 high) twice; all findings fixed and re-verified

### Milestone 5 codex findings (all fixed)

- MEDIUM: the SSE handler cleared the write deadline for the whole stream, so a
  client that stopped reading could block a write forever and the deferred
  unsubscribe never ran (subscription/goroutine leak). Each SSE write now takes a
  bounded 10s deadline and the loop returns on any write failure, so unsubscribe
  always runs.
- MEDIUM: flash cookies carrying freshly minted token/invite secrets omitted the
  Secure flag, so on an HTTPS deployment with CookieSecure set a secret could
  still ride a plain-HTTP request. setFlash now honors Cfg.CookieSecure like the
  session cookie.
- LOW: the post-login `next` target was dropped (the form posted to /login with no
  query), so a deep link always bounced to /. LoginPage now carries next as a
  hidden field, validated through safeNext on POST.
- By design (not a finding): a full-scope API token can read the HTML UI. Full
  scope is the read credential and M5 ships no JSON read API, so HTML is the only
  read surface; ingest-only tokens remain blocked. Comments updated to say it.

## Milestone 6: public publishing (DONE)

Goal: per-session public publishing. An owner-only publish/unpublish control, an
unguessable capability id minted on publish, and a logged-out session view at
`/s/{public_id}` that never exposes the numeric id or any internal data.

- [x] internal/server/auth: NewPublicID (144-bit URL-safe capability id, stored
      in the clear because the link itself is the grant)
- [x] internal/server/store/visibility.go: PublishSession (owner-scoped, mints id
      via COALESCE so re-publish keeps a stable link), UnpublishSession (clears
      visibility and public_id so the old link 404s)
- [x] internal/server/store/read.go: SessionDetail carries OwnerID; public lookup
      stays filtered to visibility='public'
- [x] internal/server/httpapi/web.go: publish/unpublish handlers (requireFull,
      owner check folded into SQL), handlePublicSession (no auth, filters
      subagents to public-only, never renders the numeric id)
- [x] internal/server/web: owner publish/unpublish control with the shareable
      link on the session page; publicLayout, PublicSessionPage, PublicErrorPage
- [x] Tests: store publish/unpublish (owner scoping, id stability, link
      invalidation), HTTP public flow (anon 303 on numeric id, 200 on public link
      with content, 404 after unpublish, no numeric-id leak in the body)
- [x] e2e: real browser publish -> public badge + shareable link -> logged-out
      public view (brand + Log in chrome only); cookie-less fetch returns 200,
      numeric id redirects to login, unknown/unpublished id 404s
- [x] codex review (gpt-5.5 high) twice; finding fixed and re-verified

### Milestone 6 codex findings (all fixed)

- MEDIUM: the public page exposed the numeric session id in the page title and
  H1 ("session #<id>"), violating the requirement that the public path never
  reveal it. PublicSessionPage now titles by project and renders "/ session" with
  no number; the HTTP test asserts neither "/sessions/{id}" nor "#{id}" appears.

## Milestone 7: CAS (DONE)

Goal: move bulky tool bodies out of the hot tables into a content-addressed store
backed by Postgres large objects, deduped across sessions, with session-scoped
serving and on-demand expansion in the UI.

- [x] internal/parser: ToolCall.ResultBody (was ResultText) holds the canonical
      body bytes; bodyContent returns body + media type so size and media always
      describe exactly what the CAS stores (text-block arrays flatten to readable
      text/plain; genuine objects stay raw JSON)
- [x] internal/server/store/blob.go: writeBlobTx (lo_create + lowrite + insert,
      deduped, race-safe via FOR KEY SHARE), BlobMeta, WriteBlobTo (streams the
      large object), SessionReferencesBlob, SweepBlobs (FOR UPDATE SKIP LOCKED)
- [x] internal/server/store/projection.go: WriteProjection stores tool input and
      result bodies in the CAS within its transaction and records the sha256
      references; reparse reuses existing blobs and orphans nothing for identical
      content
- [x] internal/server/httpapi/blob.go: GET /api/v1/session/{id}/blob/{sha256}
      (full scope) and GET /s/{public_id}/blob/{sha256} (logged out); both verify
      the session references the hash, validate the 64-hex sha, send nosniff, and
      serve only JSON/plain text inline (anything else as opaque bytes)
- [x] internal/server/web: body chips are buttons carrying a blob URL; app.js
      fetches and expands them inline with textContent (never innerHTML), on both
      the authed and public views
- [x] cmd/akari-server: sweep subcommand; reparse sweeps once projections rebuild
- [x] Tests: CAS dedup/read/sweep, the sweep-skips-a-writer-locked-blob race, and
      HTTP access control (authed and public serving, cross-session leak blocked,
      internal body unreachable via a public session, malformed sha)
- [x] e2e: reparsed live data to populate the CAS, then expanded both tool input
      and result bodies inline in a real browser (fetched from the blob endpoint)
- [x] codex review (gpt-5.5 high) twice; both findings fixed and re-verified

### Milestone 7 codex findings (all fixed)

- MEDIUM: a sweep could race a live writer: writeBlobTx returned on a plain
  EXISTS check without locking, so a concurrent SweepBlobs could delete a blob
  between the check and the referencing tool_calls insert, failing the FK.
  writeBlobTx now locks the existing row FOR KEY SHARE and the sweep claims
  orphans FOR UPDATE SKIP LOCKED, so the sweep skips a blob a writer is about to
  reference (or the writer recreates it if the sweep won). Covered by a test.
- LOW: ResetRaw drops tool_calls and attachments, orphaning blobs, without
  sweeping. Documented that these are reclaimed by a later SweepBlobs (a
  synchronous full sweep on every client reset would be a performance footgun),
  matching the design's "sweep runs after deletions or re-parses".

## Milestone 8: polish (DONE)

Goal: round out the lifecycle and the docs. Session deletion (retention), a
configurable background blob sweep, a real README, and broader parser coverage.

- [x] internal/server/store: DeleteSession cascades a session's derived rows and
      raw bytes; referenced blobs are reclaimed by a later sweep
- [x] internal/server/httpapi: POST /sessions/{id}/delete (owner or admin), with
      a delete control on the session page guarded by owner || admin
- [x] internal/config + cmd/akari-server: AKARI_SWEEP_INTERVAL (default 1h, 0
      disables); a background goroutine sweeps on the interval, cancelled cleanly
      on shutdown before the pool closes
- [x] README rewritten: what akari is, the client/server split, server and client
      setup, configuration table, maintenance subcommands, the UI, publishing,
      retention, and development
- [x] Broader parser coverage: a Claude error tool result delivered as text
      blocks (error status, flattened body, faithful size/media)
- [x] Tests: DeleteSession cascade + orphan, delete authz (non-owner 403, owner
      and admin allowed), config duration parsing
- [x] e2e: deleted a session in the browser (redirect to the now-empty project),
      confirmed the background sweep logs on startup and the sweep subcommand runs
- [x] codex review (gpt-5.5 high) twice; the shutdown race it found was fixed and
      re-verified

### Milestone 8 codex findings (all fixed)

- MEDIUM: the background sweep ran on context.Background(), so on SIGTERM the
  connection pool could close while a sweep was in flight (use-after-close). The
  sweep now runs on a cancelable root context; shutdown cancels it and waits on a
  sweepDone channel before the pool closes, covering the SIGTERM, early
  ListenAndServe error, and migrate-error paths (go vet lostcancel clean).

## Milestone 9: analytics + session redesign (DONE)

Goal: a dark industrial visual system (the Machinist's Bench, see DESIGN.md and
PRODUCT.md), inline fleet analytics, and a reworked session read experience.

- [x] Design system captured in PRODUCT.md (strategic) and DESIGN.md (visual,
      Stitch format) plus the .impeccable/design.json sidecar; the engineering
      design moved to docs/DESIGN.md
- [x] internal/server/store/analytics.go: time-bucketed usage rollups (daily
      series, by-model, by-agent) scoped to a project or the whole instance, and
      ProjectSparklines (per-project 30-day cost, bucketed in Go from one query)
- [x] internal/server/web: chart view-models (inline series JSON on a data
      attribute, breakdown bars, server-rendered sparklines, the timeline rail,
      diff-tool detection); full token system in app.css (Geist + Geist Mono
      self-hosted as woff2, lilac-on-violet-graphite, motion with reduced-motion
      fallbacks)
- [x] static/charts.js: a dependency-free SVG time-series renderer (hairline
      grid, mono ticks, lilac crosshair with a mono readout, cost/token metric
      toggle, resize redraw)
- [x] static/app.js: inline diff rendering for editing tools across the three
      agents, transcript density modes (persisted), the rail scroll spy, and the
      instrument needle-settle on live stat changes
- [x] templates + handlers: inline analytics on the index (global) and project
      pages; the session view reworked into a timeline rail plus a sticky
      instrument header and a diff-ready transcript
- [x] Tests: store analytics rollups + sparkline windowing (DB-gated); web chart
      helpers (breakdown widths, sparkline shape, rail markers, diff detection,
      series JSON round-trip); full suite green with `-p 1`
- [x] Verified in a real browser against seeded data: global and per-project
      charts with a working crosshair (cost and token modes), breakdown bars and
      sparklines, the session timeline rail and error marker, an Edit tool
      expanding as a rendered diff, the density toggle, and the logged-out public
      session view (no numeric id leaked)
- [ ] codex review (gpt-5.5 high) before merge, per the milestone convention

## Milestone 10: client-side CAS upload (DONE)

Goal: move tool-call body storage off the inline transcript and onto the CAS at
upload time, so a transcript stays small however big the tool outputs are and the
508 MiB-turn case (98% base64-image tool results) uploads at all.

- [x] internal/parser/extract.go: the single definition of which bytes are tool
      bodies, shared by client and server. ExtractBodies/RewriteLine rewrite each
      tool input/result body to a `{"__akari_cas__":1,...}` sentinel inline (span
      located via gjson Index, body bytes identical to what the reducer CAS'd) and
      surface the lifted bodies; the reducer reads the sentinel back into a CAS
      reference (claude/codex/pi + applyResult)
- [x] internal/client/upload: transform.go streams the original tail line by line,
      lifts bodies, and assembles boundary-aligned, size-bounded transformed
      chunks; upload.go uploads the bodies (check + chunked PUT) then the
      transformed chunk, resuming against the original file while the server tracks
      the transformed cursor (verify compares the transformed-prefix hash; cold
      cache re-transforms from zero to recover the offset mapping)
- [x] Server: blob check + PUT endpoints (httpapi/blob_upload.go), HaveBlobs/PutBlob
      with hash verification and a blob_pins TTL so an uploaded-but-unreferenced
      body survives the sweep; applyDelta records a reference (no blob write) for an
      uploaded body and re-locks it FOR KEY SHARE; SweepBlobs excludes live pins and
      clears expired ones; ErrBlobNotUploaded guards a dangling reference
- [x] migrations/0004_client_cas_upload.sql: blob_pins table (pre-release wipe of
      session-scoped data, identity preserved)
- [x] Tests: extraction parity (claude/codex/pi + a base64-image result: sha,
      bytes, media equal the inline set), parser round-trip (sentinels parse to the
      same projection), store pin/sweep safety + reference recording, and end-to-end
      client→server dedup-on-resync (zero bytes, zero bodies, cold cache too), big
      body (160 MiB result, transcript stays tiny), and resume; full suite green
      with `-race -p 1`

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
- Integration tests provision an isolated database per test (create, migrate, drop
  on cleanup; see internal/server/storetest), so they run at the default
  parallelism: `go test ./...`, no `-p 1`. They skip unless
  AKARI_TEST_DATABASE_URL is set; only its host and credentials are used, since
  each test's database is created beside the one it names.
- Tool-call input/result bodies are stored as size + media type only in M2; the
  CAS milestone will store the bodies themselves and back-reference them.
