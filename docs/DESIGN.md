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

**Worktrees of a local-only repo.** A repo with no `origin` cannot collapse by
remote, so its worktrees would otherwise each become a separate standalone
folder. The same `commondir` that backs the remote case gives a high-confidence,
non-heuristic fallback: `git -C <cwd> rev-parse --git-common-dir` resolves to the
one `.git` shared by every worktree and the main checkout, so its parent (the
main worktree) is a single root they all agree on. A standalone session in a live
worktree reports that root, and the server keys the local project on it, so a
local-only repo's worktrees collapse just like a remote-backed one's. This is
best effort: it needs a live worktree git can still inspect, so a worktree that
has already been archived (its checkout removed) cannot be matched back, its
session metadata records only `cwd` and the branch, never the parent repo, and we
do not guess from the path.

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
synthetic (`local:<machine>:<location>`), where the location is the repo root
shared by a live worktree (see "Worktrees of a local-only repo") when one is
reported, and otherwise the session's `cwd`. Every standalone or orphaned session
that shares that location on the same machine groups into one project. Standalone
and orphaned share the key namespace, so a folder deleted while keyed on its
`cwd` transitions from standalone to orphaned in place (its kind flips) rather
than forking. A worktree that was grouped under a repo root while live, then
archived, can no longer report that root and so pops out into its own
location-keyed project: the live repo group is unaffected, and an archived
worktree has no reliable parent signal to recover anyway.

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
  input body and the tool result body, are not stored inline: each lives in the
  content-addressed store and is referenced by hash, with its size and media type
  kept on the row. The client lifts these bodies out of the transcript and uploads
  them to the CAS directly (see Ingest protocol), so a giant tool output never
  travels inline; the server records the reference rather than re-storing the body.
  The UI shows them as metadata first (for example "36 KB json") and fetches the
  body from the CAS only when the user expands it.
- **usage_events**: token accounting rows (input, output, cache-creation a.k.a.
  cache-write, cache-read, reasoning) with computed cost, keyed for dedup.

These power reading and stats. They are derived data: the server can drop and
rebuild them from the stored raw bytes whenever the parser improves.

## Server

### Responsibilities

1. Ingest raw session bytes over HTTP (resumable, idempotent).
2. Store the transformed transcript permanently as the lossless backup and
   re-parse source (tool bodies live in the CAS, referenced by sentinels).
3. Parse the transcript into the queryable projection (messages, tool calls,
   usage), recording each tool body's CAS reference from its sentinel.
4. Compute token stats and cost.
5. Accept content-addressed uploads of tool input/result bodies (and store binary
   attachments) in the large-object store, deduped by hash.
6. Serve a server-rendered web UI and a small read API.
7. Authenticate users and tokens; enforce the internal/public boundary.

### Ingest protocol

All ingest endpoints require `Authorization: Bearer <token>`. The unit of upload
is the session file, but it is not uploaded raw. Before it is sent, the client
lifts every tool input and result body out of the transcript, uploads those
bodies to the content-addressed store directly, and rewrites each body inline as a
compact CAS sentinel. The server stores and parses this **transformed**
transcript, which stays small however big the tool outputs are. A single 508 MiB
Codex turn that is almost entirely base64-image tool results becomes a small
transcript of sentinels plus many image blobs, so it uploads where an inline
transcript could not (it would exceed the 128 MiB chunk cap).

The transcript is streamed incrementally by byte offset. The cursor is the number
of TRANSFORMED bytes the server has stored (`stored_bytes`), and these files only
ever grow by appending, so the protocol is built around append-only growth with an
explicit divergence check.

**Sentinel format.** Each tool input or result body is replaced inline by a
single-line JSON object:
`{"__akari_cas__":1,"sha256":"<hex>","bytes":<n>,"media_type":"<type>"}`. The
`__akari_cas__` key namespaces it so no real tool body collides. The rewrite
happens strictly inside the body's JSON value span, so the line keeps its newline
and a Codex turn-closing user line keeps its shape: the transformed stream has the
same line and turn boundaries as the original, which is what keeps it resumable and
turn aligned. `sha256` is the CAS key, the hash of the STORED bytes the CAS holds
(the raw body for a small one, the zstd-compressed form for a large one: see
Compression under CAS), not the raw body, so the transcript references exactly the
bytes the CAS serves. `bytes` is the RAW body length, the size the row and UI
report, kept independent of how the bytes are stored. `media_type` is the body's
semantic type. A tool-input sentinel also carries two optional fields the reducer
projects onto the tool call, because lifting the body would otherwise erase them:
`file_path` (the input's top-level file path), and `detail` (a bounded
human-scannable summary of the input: a command, pattern, URL, or description the
UI shows when a call has no file_path). The extraction and the sentinel have one
definition in `internal/parser`, used by both the client (to lift and rewrite) and
the server reducer (to interpret), so the client-uploaded body set can never drift
from what the server records.

**Resume model.** The client still resumes by the ORIGINAL on-disk file, because
that is all it can recompute statelessly (offset plus prefix hash). But the bytes
the server holds are the transformed transcript, so the announce handshake
compares the client's TRANSFORMED prefix hash against the server's. The client
caches, per file, the verified transformed offset, a resumable sha256 of the
transformed prefix, and the original offset that maps to it; each tick transforms
only the newly appended original tail, so steady-state work is proportional to the
appended bytes. A dropped cache (a restart) re-transforms the original file from
zero once to recover the prefix digest and the offset mapping, the same class of
cost as the old cold re-hash, and never re-uploads a body. The transform is
deterministic and line aligned, so the recomputed prefix is byte identical to what
was uploaded and the stream stays append-only. Server reparse-from-stored-raw
still works: the stored raw is the transformed transcript, and the parser fills
each tool body's reference from its sentinel rather than the CAS.

1. **Announce / upsert session.**
   `POST /api/v1/ingest/session`
   ```json
   {
     "agent": "claude",
     "source_session_id": "0e3b...uuid",
     "project_remote": "github.com/jssblck/akari",
     "git_branch": "main",
     "cwd": "/home/grace/projects/akari",
     "machine": "grace-laptop",
     "terminal": false
   }
   ```
   `terminal` (default false) is the client's assertion that the session is
   finished, set by `akari sync --finalize` on an ephemeral host. The server
   persists it sticky (a later ordinary re-announce never clears it) and ORs it into
   the two server-side idle checks that gate grading, so a terminal session is
   gradeable immediately rather than after the 30-minute abandoned-idle window. The
   server upserts the project and session rows (latest announce wins
   for mutable metadata like `git_branch` and `cwd`) and replies with the session id,
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

   The client keeps the hash incremental so a long session does not re-hash its
   whole prefix on every sync. It caches, per file, the verified offset and a
   resumable sha256 digest of `[0, verified)`. An append-only file whose prefix is
   already cached is confirmed by comparing the cached digest (no I/O); a file the
   server has grown past the cache is confirmed by hashing only the new span; and
   after a successful append the digest advances over the bytes just sent, which
   the client already holds. Only a cold cache, a server rewind, or a truncation
   (a file shorter than the cache describes) falls back to a full re-hash. The
   cache is an in-memory accelerator: losing it costs one re-hash, never
   correctness, since the server's `prefix_sha256` remains the sole authority on
   divergence.

2. **Upload tool bodies (CAS).** Before sending a transformed chunk, the client
   ensures the CAS holds every body that chunk references. Encoding is the client's
   job, not the server's: a body at or above a size threshold is zstd-compressed,
   a smaller one is left raw (see Compression under CAS), and the CAS key is the
   sha256 of the resulting STORED bytes. To learn the key the client streams the
   body through the encoder once, hashing the output, then checks and (if missing)
   re-streams it for the upload, a deliberate second compression pass it is happy
   to pay to keep the server off the compression CPU path. Building keys for many
   bodies at once (a fleet of files, or one large catch-up sync) is bounded to the
   CPU count by a shared semaphore on the encoder, so the compression never
   oversubscribes the machine.

   Rather than check and upload each body inline as it is found, the client lifts a
   body's key immediately (so it can write the chunk's sentinel) but defers the
   round-trip: bodies are queued and ensured present in batches, before the chunk
   that references them is sent and once more at the end of a pass (so a body lifted
   from a withheld trailing turn is uploaded the tick it is first transformed, since
   the held lines are cached and never re-transformed). A bound on the in-hand body
   bytes held forces an early batch so memory stays flat regardless of how many
   bodies a chunk references.
   - `POST /api/v1/ingest/blobs/check` with `{"sha256":[...]}` returns
     `{"missing":[...]}`, the keys the CAS does not yet hold. The client sends at
     most 100 hashes per request so the server's per-request work is bounded, and
     fans the requests out in parallel for a large queue. The CAS dedupes globally,
     so a body any session already stored (this one on an earlier sync, or any
     other) is reported present and never re-sent, and is pinned by the check so it
     survives until the referencing chunk lands. This is what makes a re-sync of an
     unchanged file upload zero bodies. Because the encoding is deterministic, the
     same body always yields the same key, so dedup holds.
   - `PUT /api/v1/ingest/blob/{sha256}?media_type=<type>&content_type=<enc>`
     streams one body's stored bytes to the CAS, where `content_type` is the
     storage encoding (`application/octet-stream` raw or `application/zstd`). The
     missing bodies upload in parallel under an adaptive concurrency limiter that
     walks the in-flight width from observed upload latency (shrinking when the
     server sheds load or the network saturates, growing when it is healthy). The
     server streams the bytes to the large object in bounded slices and verifies
     they hash to the path's sha256, so a corrupt or mislabeled upload is rejected
     rather than poisoning the store; it never decompresses. Each upload pins the
     blob against the sweep for a TTL (see CAS), so a body cannot be reclaimed in
     the window between uploading it and uploading the transcript that references
     it.

   Bodies are ensured present before the transcript that references them, so the
   parse can always resolve a sentinel to a present blob; a sentinel whose body is
   somehow absent leaves the parse cursor for a retry rather than recording a
   dangling reference.

3. **Append bytes.**
   `POST /api/v1/ingest/session/{id}/chunk?offset=40960`
   with the transformed bytes from that offset as the body. A chunk ends on a message
   boundary, never inside a message. For Claude and pi a message is one JSONL
   line, so a chunk ends on the last `\n`. For Codex a message is a folded turn
   (reasoning, tool calls, and the assistant reply), so a chunk ends on a turn
   boundary: the client cuts right after a user line, which is where a turn
   closes. Turn alignment is a client nicety, not something the server depends
   on: the parser always refolds the whole stored session, so a turn whose bytes
   spanned two chunks parses correctly once the rest lands. Line alignment is
   the invariant that matters, and the server enforces it. The
   client prefers ~1 MiB transformed chunks; because each tool body is lifted to
   the CAS, a transformed line (and so a chunk) stays small even for a turn whose
   original bytes are enormous. A single transformed line past a 128 MiB cap is
   refused, but after the bodies are lifted that only happens for a truly
   pathological line. Boundary detection runs on the transformed bytes, which is
   sound because the transform is line aligned (it preserves every newline and
   every Codex turn close). The final turn of a session has no closing user line,
   so the client withholds it until the file goes idle (it has not changed for a
   settle window), then flushes it whole. The server:
   - Rejects with the current length if `offset` does not equal `stored_bytes`
     (idempotent: a re-sent chunk whose offset is behind is a no-op that returns
     the truth, so the client simply advances).
   - Rejects a chunk that is empty or does not end on a newline, so the line
     boundary the parser relies on is a server-enforced invariant, not just a
     client convention. (Turn alignment is not re-checked: a turn split across
     chunks at worst renders partially for a moment and correctly once the rest
     lands and the session rebuilds.)
   - Appends the chunk as a new raw row and advances the content hash by resuming
     its stored digest state over only the new (transformed) bytes. Both are one
     transaction: once it commits, the upload has succeeded regardless of what
     parsing does.
   - Wakes the parse worker. Parsing never runs on the request: the worker
     rebuilds the session's whole projection from the stored bytes in the
     background (see "Server-side parsing pipeline") and publishes the SSE
     update when the rebuild commits. The append itself is what marks the
     session due (its `byte_len` moves past the last rebuilt length), so a chunk
     that lands mid-rebuild is picked up by the next one; the wake buys latency,
     not correctness. Each tool body's sentinel is recorded as a CAS reference
     (sha256, bytes, media type) with no blob write, since the client already
     uploaded the body. Parsing stays best effort: a parse failure leaves the
     durable bytes in place for a later rebuild to retry, so client ingest
     health never depends on parser correctness.
   - Returns the new `stored_bytes`.

4. **Reset.** `POST /api/v1/ingest/session/{id}/reset` truncates the raw store
   (its chunks, length, and hash) and drops the derived rows; the next chunk
   rebuilds the projection from zero. The client calls this when the announce
   divergence check fails.

5. **Finalize.** `POST /api/v1/ingest/session/{id}/finalize` grades a terminal
   session now, rather than leaving it for the parse worker's settle tick.
   `akari sync --finalize` calls it once a session's whole transcript has landed:
   the session was announced terminal, so its signals derive with the idle checks
   satisfied. The grade reads whatever projection is current; if the final chunks'
   rebuild is still draining, that rebuild re-grades the session when it commits
   (a rebuild always grades a terminal session), so finalize never needs to parse
   inline and the ephemeral host never needs to wait: the raw bytes are already
   durable and the server finishes on its own. It carries no body and is
   idempotent: a non-terminal session simply grades under the ordinary rules.

Because the server stores raw bytes and `stored_bytes` is the cursor, there is
no separate client-visible sync watermark to keep coherent: the server is always
the source of truth for "how much of this file do you have, and does it still
match mine."

### Server-side parsing pipeline

Parsing is decoupled from ingest and has exactly one shape: rebuild the whole
session. A background worker inside the server process owns every projection
write; the ingest path never parses. There is no incremental parse, no
serialized parser state, and no separate reparse mechanism. A session that
gained bytes, a session whose last parse failed, and a corpus behind a new
parser epoch are all the same case ("the projection is behind the raw bytes")
handled by the same code: refold the stored bytes from zero and swap the result
in atomically.

**Dirty tracking.** `session_raw` carries two bookkeeping columns beside the raw
cursor: `parsed_byte_len`, the raw length the last successful rebuild covered,
and `parser_epoch`, the `parse.Epoch` it ran at. A session is due when
`parsed_byte_len <> byte_len` or when `parser_epoch` is behind the running
binary's constant. The epoch comparison is monotonic on purpose: a session
stamped ahead of the running epoch (a newer binary's work, seen by the older
binary during a rolling deploy) is never due, even when byte-dirty, so an old
worker cannot rebuild it back down to the older parser; the newer instance
picks up its appends on its next wake or tick. An append makes a session due
by construction (it grows
`byte_len`), so there is no flag to race on: a chunk that lands while a rebuild
is committing leaves the comparison unequal and the session is simply rebuilt
again. A deploy with a bumped epoch makes the whole corpus due the same way,
which is why "reparse everything" is not a separate mechanism.

**The reducer sees the whole session.** The per-agent parser is a reducer fed
the session's complete stored bytes, streamed chunk by chunk within one parse
call. Because a parse always starts at byte zero, the reducer's working state
(the next ordinal, the sticky model, an open turn being folded) lives in
ordinary memory for the duration of the call and is never serialized. That is
what lets a fold cross line and chunk boundaries: Codex turns fold as before,
and Claude's content-block lines (one API assistant message logged as separate
thinking / text / tool_use JSONL lines sharing a `message.id`) fold into a
single assistant turn, so a `messages` row is one semantic turn for every agent
(issue #98). A tool result back-patches its call inside the same in-memory
fold, matched by call id; when a resumed or compacted transcript replays a call
id onto several rows, every copy receives the result.

**The rebuild is one transaction.** `store.RebuildSession` locks the session
row and its raw row (the same order the delete path takes), reads `byte_len` as
the rebuild's high-water mark, folds the full projection in memory, deletes the
old projection rows, and bulk-inserts the new set (`CopyFrom`). Everything
derived is computed in that fold, over complete information and into empty
tables: usage dedup (Claude's repeated usage blocks collapse in memory, not via
ON CONFLICT arithmetic), per-event pricing at each event's `occurred_at`, the
session rollups (summed from the exact row set being written, so the
rollup/ledger invariant in docs/data-aggregation.md holds by construction), the
per-turn usage rollup, prompt-hygiene facts and the duplicate-prompt flag
(judged against the in-memory ordered prefix), relative tool paths against the
session's current cwd, cache savings, and model-fallback merging. The session's
blobs are pinned before the delete so a concurrent CAS sweep cannot reclaim a
still-referenced body. When the session is settled or terminal, its signals
recompute in the same transaction (see docs/signals.md); a rebuild of a
still-live session instead drops the now-stale signals row and leaves grading
for the settle tick. Readers never see a half-built session: the old projection
serves until the commit, then the new one does. Peak memory is proportional to
the transformed transcript, which the CAS lifting keeps small however large the
original tool bodies were.

**Lock ordering.** Concurrent rebuilds, ingest, and the CAS sweep stay
deadlock-free by taking shared rows in one global order. A rebuild locks its
session row before its raw row (the order the delete and reset paths share)
and writes the sessions row exactly once per transaction. That single-write rule exists because
Postgres re-fires a row's foreign-key checks when a transaction updates the
row a second time, even with the key columns unchanged; on a subagent session
the re-check takes FOR KEY SHARE on the parent session's row, which closed a
cycle against a concurrent rebuild of the parent blocked on shared blob pins
(the 2026-07 epoch-drain deadlock, reproduced by
TestConcurrentRebuildDeadlockStress). Every writer that locks multiple
blob_pins or blobs rows takes them in sha order: the rebuild's session-wide
pin, the client-CAS check-and-pin, and new-blob inserts. The sweep's
expired-pin reap uses SKIP LOCKED instead of waiting, since a pin row locked
mid-refresh will be unexpired by the time the lock clears. If a cycle ever
slips through regardless, the deadlock surfaces as an ordinary operational
error: Postgres aborts one transaction, that rebuild rolls back whole, and the
worker parks and retries it under the failure model below. The backstop is
retry, never a torn projection.

**Failure model.** A reducer error on malformed bytes is deterministic
(re-running fails identically), so the worker rolls the rebuild back and
records the attempt on the failure markers: `session_raw.parse_error` plus the
epoch and raw length the attempt covered. The last-successful-rebuild
bookkeeping (`parsed_byte_len`, `parser_epoch`) is deliberately left alone, so
the surviving projection keeps reading as the epoch that actually built it
rather than masquerading as current, and the session's signals flip stale (the
settle pass regrades them from the surviving projection under the current
scoring). The due scan skips the session while the recorded failure covers its
current bytes at the running epoch or ahead (an attempt stamped ahead belongs
to a newer binary and is off-limits the same way a newer success is), so it
neither hot-loops on the same bad bytes nor goes silent forever: new bytes or
a bumped epoch retry it. Every staleness surface shares one indexed
expression, the attempted epoch (the last successful rebuild's epoch, raised
to the failure epoch while the failure still covers the current bytes): the
due scan's epoch branch, the fleet progress count, and the OG-card snapshot
check all test it against the running epoch, so "due" and "gated" can never
drift apart, and a corpus full of pinned failures indexes AT its failure
epochs, outside the behind-range the hot probes scan, so probe cost tracks
the actual backlog rather than the accumulated failure history. New bytes
break the pin and readmit the session to the scan and the gates in the same
instant. An operational error (a store or CAS failure, a shutdown) records
nothing about the parse, but the worker defers the session's next attempt with
a doubling backoff (30s to a 1h ceiling): the session must not fall out of the
due set (the failure may clear on its own), yet a persistent one (a CAS blob
the client never uploaded) must not be re-attempted on every chunk wake, since
each wake drains the whole due set. The deferral is indexed the same way the
failure pin is: parked rows leave the ready-work indexes the due scan and the
drain's opening count read (an elapsed retry comes back via one range scan on
its ready time), so a parked backlog costs the hot paths nothing. New bytes, a
reset, or an operator reparse clear the deferral for an immediate retry; the
epoch gates ignore it (deferred is not done).

**Scheduling.** The worker drains due sessions continuously, woken in-process
by the chunk handler and backstopped by the periodic maintenance tick that also
grades settled sessions (`AKARI_SIGNALS_SETTLE_INTERVAL`). Live-session latency
is one wake plus one rebuild, so SSE viewers see a session grow within a moment
of its chunks landing, and a burst of chunks coalesces into a few rebuilds
instead of paying a parse per chunk. Distinct sessions rebuild on a small pool;
two rebuilds of one session serialize on its row locks. Multiple server
instances need no coordination. At one epoch, two rebuilds of a session are
identical, so whichever commits last wins. Across a rolling deploy the
monotonic due predicate keeps the binaries out of each other's way: the older
binary skips sessions the newer one already stamped, the newer binary sees the
older stamps as due, and the corpus converges on the newest epoch because the
newest binary outlives the rest.

**The epoch.** `parse.Epoch` is the single version constant for everything
derived from raw bytes. Bump it in the same commit as any change to parser or
reducer output, a rebuild-derived column, the signal set or scoring, prompt
classification, or the pricing table. The next deploy sees the corpus due and
rebuilds it in the background; nothing else needs versioning, because nothing
derived can stay behind for longer than one rebuild. It is a binary constant
and not a migration on purpose: parser behavior lives in the binary, and a
parser change often ships with no schema change at all. The golden-fixtures
test (`internal/server/parse/epoch_test.go`) snapshots the projection for
representative sessions and fails, naming `parse.Epoch`, if output drifts
without a bump, so the bump cannot be forgotten.

**Fleet rebuilds and UI gating.** An epoch rollout rebuilds the corpus one
session at a time, so a cross-session view could briefly mix old and new
sessions. While a fleet rebuild is draining, the server gates the parsed pages
behind a progress view (pushed over SSE, with `GET /api/v1/reparse/status` as
the poll fallback); raw-data, auth, and account endpoints stay available. The
admin Reparse button (`POST /account/reparse`) and the
`akari-server reparse [--agent claude]` CLI remain as manual triggers, now
implemented as "mark the scope due" against the same worker rather than a
parallel service.

**Migration from the incremental pipeline.** The raw store and the CAS carry
over unchanged; everything derived is rebuilt. One migration drops the
incremental bookkeeping (`session_raw.parse_state` and `parse_state_version`,
`sessions.parser_version` and `cache_savings_backfilled`, the `parse_meta`
table, and the `signals_version` / `prompt_facts_version` stamps on
`session_signals` and `messages`) and adds `session_raw.parser_epoch DEFAULT
0`. Every session then reads as due on first boot and the corpus rebuilds
through the ordinary worker, which re-derives columns, re-grades signals, and
re-prices usage in the same pass, so there is no one-off backfill step.

Per-agent specifics the parser must handle:

- **Claude Code** (`~/.claude/projects/<slug>/<id>.jsonl`): newline-delimited
  JSON; messages carry `uuid`/`parentUuid` (a DAG, with forks), `cwd`,
  `gitBranch`, `message.content`, and per-message token usage with
  `input_tokens`, `output_tokens`, `cache_read_input_tokens`,
  `cache_creation_input_tokens`. Each content block of one API assistant
  response (its thinking, its text, each `tool_use`) is logged as its own JSONL
  line sharing the response's `message.id`; the reducer folds those lines into
  one assistant turn, so a `messages` row is a turn, not a content block, and
  the shared usage block prices once. Subagent runs live under `subagents/`.
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
binary: a map of canonical model ID to per-million-token rates for input, output,
cache-write, and cache-read. Matching is exact (a key prices only its own model,
never a family), and each model maps to a list of date-effective rates so one ID can
price pre-change and post-change usage differently (an introductory promo that
reverts, a mid-life reprice); the usage event's time selects the window in effect
when it occurred. There is no runtime catalog or refresh endpoint; updating prices
means a new build. The computed cost is stored on each usage event.

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
  scope        TEXT NOT NULL DEFAULT 'ingest',  -- ingest | read | full
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
  remote_key   TEXT NOT NULL UNIQUE,      -- remote: github.com/jssblck/akari; local: local:<machine>:<location>
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
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),  -- row write time; moves on reparse
  -- Feed recency: the session's last event time, falling back to created_at for a
  -- transcript with no timestamps. Generated (not written), so a reparse that
  -- restamps updated_at leaves it fixed. This is what the lists display and order
  -- by, so an old session no longer reads as "updated" today (migration 0033).
  last_active_at    TIMESTAMPTZ NOT NULL GENERATED ALWAYS AS (COALESCE(ended_at, created_at)) STORED,
  UNIQUE (user_id, agent, source_session_id)
);
CREATE INDEX idx_sessions_project ON sessions(project_id);
CREATE INDEX idx_sessions_user    ON sessions(user_id);
CREATE INDEX idx_sessions_public  ON sessions(id) WHERE visibility = 'public';
CREATE INDEX idx_sessions_parent  ON sessions(parent_session_id)
  WHERE parent_session_id IS NOT NULL;

-- Raw bytes: lossless backup and re-parse source. Append-only. The parent row
-- holds the cursor, the prefix hash and its resumable digest state, and the
-- rebuild bookkeeping (a session is due for a rebuild when parsed_byte_len <>
-- byte_len or parser_epoch is behind the binary's parse.Epoch); the bytes
-- themselves are appended as chunk rows so growth is O(append), never a
-- detoast-and-rewrite of the whole value.
CREATE TABLE session_raw (
  session_id      BIGINT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
  byte_len        BIGINT NOT NULL DEFAULT 0,    -- == sessions cursor, line-aligned
  content_sha256  CHAR(64) NOT NULL DEFAULT '...', -- sha256 of all bytes; the prefix hash
  sha256_state    BYTEA,                        -- resumable digest, so hashing is O(append)
  parsed_byte_len BIGINT NOT NULL DEFAULT 0,    -- raw length the last successful rebuild covered
  parser_epoch    INT NOT NULL DEFAULT 0,       -- parse.Epoch that rebuild ran at
  parse_error     TEXT NOT NULL DEFAULT '',     -- last deterministic parse failure, '' when clean
  parse_error_epoch    INT NOT NULL DEFAULT 0,     -- epoch that failure was attempted at
  parse_error_byte_len BIGINT NOT NULL DEFAULT 0,  -- raw length that failure covered
  parse_retry_at           TIMESTAMPTZ,            -- operational-failure backoff: due scan skips until then
  parse_retry_backoff_secs INT NOT NULL DEFAULT 0, -- doubles per consecutive failure, 30s..1h
  CHECK (parsed_byte_len <= byte_len)
);
-- The attempted epoch (parser_epoch, raised to parse_error_epoch while the
-- failure covers the current bytes): one range over this expression answers
-- every epoch-staleness probe, with pinned failures indexed out of the range.
CREATE INDEX idx_session_raw_attempted_epoch ON session_raw (
  (CASE WHEN parse_error <> '' AND parse_error_byte_len = byte_len
        THEN GREATEST(parser_epoch, parse_error_epoch)
        ELSE parser_epoch END)
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
  content_length INT GENERATED ALWAYS AS (octet_length(content)) STORED,
  -- A row is one semantic turn (Claude's split content-block lines fold by API
  -- message id). Rows are only ever written by a whole-session rebuild, so there
  -- is no "still accumulating" state to track.
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
  detail            TEXT,                  -- bounded input summary (command/pattern/url/description); NULL is unmeasured
  -- Bulky bodies live in the CAS; the row keeps only references and metadata.
  input_sha256      CHAR(64) REFERENCES blobs(sha256),
  input_bytes       BIGINT,
  input_media_type  TEXT,                  -- e.g. application/json
  result_sha256     CHAR(64) REFERENCES blobs(sha256),
  result_bytes      BIGINT,
  result_media_type TEXT,
  result_status     TEXT,                  -- ok | error | (empty if pending)
  call_uid          TEXT,                  -- agent's call id; back-patches the result by UPDATE
  PRIMARY KEY (session_id, message_ordinal, call_index)
);
-- Indexed per session for the result back-patch (UPDATE ... WHERE call_uid = $1),
-- not unique. A call id is usually unique within a session, but a resumed or
-- compacted Claude transcript replays prior assistant turns verbatim, so the same
-- tool_use id can ride more than one row. A unique index turned that into a parse
-- abort (the second insert rolled back the whole transaction); see migration 0010.
-- With it non-unique, every replayed copy keeps its id and the back-patch stamps the
-- same result onto each, and the session view flags any session that carries a
-- duplicate id so a genuinely malformed reuse is visible rather than silent.
CREATE INDEX idx_tool_calls_call_uid ON tool_calls(session_id, call_uid)
  WHERE call_uid IS NOT NULL;
-- Pending-only companion for the back-patch (UPDATE ... WHERE call_uid = $1 AND
-- result_status IS NULL). A row leaves this index once its result lands, so a
-- repeated id is back-patched by probing only the copies still pending, keeping the
-- work linear instead of re-scanning every accumulated copy on each replayed result.
CREATE INDEX idx_tool_calls_pending_result ON tool_calls(session_id, call_uid)
  WHERE call_uid IS NOT NULL AND result_status IS NULL;

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
-- Dedup happens in the rebuild's in-memory fold; the unique indexes remain as
-- integrity backstops so a fold bug fails loudly instead of double-counting.
CREATE UNIQUE INDEX idx_usage_source ON usage_events(session_id, source_offset, source_index)
  WHERE source_offset IS NOT NULL;

-- Rebuild-derived companions to the projection, written in the same rebuild
-- transaction as the rows above (sketched; migrations are authoritative):
--   message_turn_usage: per-turn token estimates behind the thinking signals.
--   model_fallbacks: merged fallback observations (see "Model fallbacks").
--   session_signals: per-session graded signals, written by the settle pass
--     (docs/signals.md).
--   session_facets: per-session facet values the session-list filters read.

-- Insights rollups (migration 0048): per-session pre-aggregations the /insights
-- page and the project quality band read instead of scanning messages,
-- tool_calls, and usage_events per render. Derived by rebuildTx in the same
-- transaction as the projection (internal/server/store/rollups.go), so they are
-- exactly as fresh as the projection with no refresh cadence; keyed on
-- session_id only, with project/user/agent/machine/outcome joining in at read
-- time. docs/insights-rollups.md has the full design.
CREATE TABLE session_usage_daily (      -- usage per (UTC day, model); day NULL = undated
  session_id BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  day DATE, model TEXT NOT NULL,
  input_tokens BIGINT NOT NULL, output_tokens BIGINT NOT NULL,
  cache_read_tokens BIGINT NOT NULL, cache_write_tokens BIGINT NOT NULL,
  cost_usd DOUBLE PRECISION NOT NULL, unpriced BOOLEAN NOT NULL
);
CREATE TABLE session_tool_rollup (      -- deduped calls/failures per (tool, category)
  session_id BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  tool_name TEXT NOT NULL, category TEXT NOT NULL,
  calls INT NOT NULL, failures INT NOT NULL,
  PRIMARY KEY (session_id, tool_name, category)
);
CREATE TABLE session_file_churn (       -- deduped edits per worktree-invariant path
  session_id BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  churn_path TEXT NOT NULL, edits INT NOT NULL,
  PRIMARY KEY (session_id, churn_path)
);
CREATE TABLE session_turns (            -- one row per measured prompt-to-reply cycle
  session_id BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  turn INT NOT NULL, prompt_at TIMESTAMPTZ NOT NULL,
  response_secs DOUBLE PRECISION NOT NULL,
  PRIMARY KEY (session_id, turn)
);
CREATE TABLE session_activity_hourly (  -- messages, tool calls, active seconds per UTC (day, hour)
  session_id BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  day DATE NOT NULL, hour SMALLINT NOT NULL,
  messages INT NOT NULL, tool_calls INT NOT NULL,
  active_seconds DOUBLE PRECISION NOT NULL,
  PRIMARY KEY (session_id, day, hour)
);

-- Content-addressed store (Postgres large objects): anything too large to inline
-- (binary attachments, bulky tool input/result bodies), deduped by content hash.
-- The bytes are stored exactly as uploaded; the server never (de)compresses them.
CREATE TABLE blobs (
  sha256       CHAR(64) PRIMARY KEY,        -- key: sha256 of the STORED bytes
  lo_oid       OID NOT NULL,               -- pg_largeobject id
  byte_len     BIGINT NOT NULL,            -- stored (possibly compressed) size
  media_type   TEXT NOT NULL DEFAULT 'application/octet-stream', -- body's semantic type
  content_type TEXT NOT NULL DEFAULT 'application/octet-stream', -- storage encoding: octet-stream | zstd
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
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

Tool bodies enter the CAS by client upload, ahead of the transcript that
references them (see Ingest protocol): the client encodes each body, asks which
keys the server lacks, and streams the missing ones to
`PUT /api/v1/ingest/blob/{sha256}`. The server streams the body to a large object
in bounded slices, verifies it hashes to the declared key, and inserts the `blobs`
row under `ON CONFLICT (sha256) DO NOTHING`, recording the storage `content_type`
the client declared. Binary attachments are written the same way server-side.
Writes never touch a count, and the server never (de)compresses.

**Compression.** Stored bytes are compressed, but the work and the policy live on
the client, never the server. A body at or above a size threshold is
zstd-compressed; a smaller one (where the frame overhead would not pay off) is
stored raw. The CAS key is the sha256 of the STORED bytes, so the server stays a
dumb byte store: it hashes whatever bytes arrive, compares them to the declared
key, and stores them, never spending CPU on compression (a hugely CPU-bound
operation we deliberately keep off the server). The encoding is deterministic (a
single fixed zstd configuration, single-threaded so block boundaries are stable),
and the size threshold sits far below the client's big-line streaming threshold,
so a body encodes to the same key whether it was buffered in memory or streamed
from disk: identical bytes always dedup. The `content_type` column records the
encoding (`application/octet-stream` or `application/zstd`) so a reader knows how
to decode; `byte_len` is the stored (compressed) size, while the raw body size is
the `bytes` the sentinel and `tool_calls` carry. The encoder lives in
`internal/casenc` (client side only), so the server binary never even links a
compression library on its hot path; the parser, which the server does link, stays
compression-agnostic and takes the encoder as an interface.

A body uploaded this way is not yet referenced by any row, so a naive sweep would
reclaim it in the gap before its transcript lands. Each upload therefore records
(or refreshes) a row in `blob_pins` with an expiry of now + a TTL (one hour). The
sweep clears expired pins first, then excludes any blob with a live pin from its
orphan set, so a freshly uploaded body survives the upload-then-reference window
(and a crash inside it) but a body whose transcript never arrives is eventually
reclaimable. Once a `tool_calls` row references the body, that reference keeps it
alive and the pin lapses harmlessly. When the parser records a reference for a body
the client uploaded, it re-locks the blob `FOR KEY SHARE` in the same transaction,
the same guard an inline write takes, so a sweep cannot delete the body between the
check and the referencing insert.

Liveness is not tracked with a refcount; it is computed when needed. Deleting a
session simply cascades its referencing rows away, and a re-parse drops and
rewrites them. A sweep then deletes any blob that no referencing row points at and
no live pin protects (`lo_unlink` plus row delete, for blobs where `NOT EXISTS` an
`attachments` or `tool_calls` reference and `NOT EXISTS` an unexpired `blob_pins`
row). This makes ingest cheaper (no counting) and the scheme self-healing (a
drifted count can never strand or prematurely free a blob). The sweep needs to run
after deletions or re-parses (which orphan referenced blobs) and to clear expired
pins of bodies whose transcripts never arrived.

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
and never exposes the numeric session id on the public path. The server streams the
stored bytes untouched: `Content-Type` is the body's semantic `media_type`, and a
zstd-stored blob is served with `Content-Encoding: zstd` so the browser (or any
client) decompresses it transparently while the server spends no CPU decoding it.

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
  the CAS, and a call's file path or, absent one, its bounded detail summary
  (a command, pattern, or URL) shows alongside the chip and in the outline
  step for that call. Any subagent sessions are shown nested under the call
  that spawned them. A publish/unpublish control for the owner.
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
  scoped `ingest` (push only), `read` (read only, for the MCP surface), or `full`
  (push and read).
- Registration requires a valid, unredeemed invite token (stored as a sha256
  hash, single use). The first-ever account bootstraps without one and becomes
  admin; every later account must redeem an invite an admin issued.
- Authorization checks, and only these: ingest endpoints require a push token
  (scope `ingest` or `full`); UI reads require a browser session or a `full` token,
  or that the specific session is public (for logged-out reads); the MCP endpoint
  accepts a `read` or `full` token (or an OAuth access token). Owner-only actions
  are limited to publish/unpublish and token management.

### Remote MCP server

The server hosts a remote [MCP](https://modelcontextprotocol.io) endpoint at `/mcp`
so coding agents can read the corpus without the UI. Two decisions shape it:

- **The protocol layer is the official Go SDK** (`modelcontextprotocol/go-sdk`),
  speaking Streamable HTTP. One server instance holds the read tools; the calling
  user rides each request's bearer token, attached to the tool handler as
  `TokenInfo`. The tools are thin: each maps to an existing `store` read and
  reshapes the result into a stable, snake_case DTO, so the MCP surface never grows
  its own query logic and stays decoupled from internal renames. The raw underlying
  data the UI fetches on demand is exposed too: tool-call bodies from the CAS
  (gated by a session that references the hash, the same gate the UI enforces) and
  a session's lossless ingested bytes, both size-capped.
- **Streamable HTTP request bodies have a 100 MiB hard limit.** A declared
  oversized body is rejected before its first byte is read; a chunked body is
  rejected after the first byte beyond the limit. Bodies larger than 1 MiB spill
  into owner-only temporary files, with at most four live spools (400 MiB of
  reserved temporary storage). Advisory lock sidecars let a new process remove
  files abandoned by a crash without touching a spool owned by an old process
  during a rolling restart. The official Go SDK v1.6.1 has no streaming or
  file-backed JSON-RPC parser hook and calls `io.ReadAll` itself, so it still
  copies each accepted request into memory. The application limit bounds that
  copy at 100 MiB. The pre-reader stops the socket at the ceiling, while the
  disk layer owns cleanup on success, protocol error, cancellation, and restart.
- **Oversized tool results need a client-resolvable resource.** MCP's
  `resource_link` content is the compatible reference shape: akari can return an
  authenticated HTTPS URI plus media type and size if a tool later needs an
  artifact endpoint. A server-local file URI is never returned to a remote
  client. Current tools instead page transcripts and cap raw or CAS body reads at
  8 MiB, so no artifact endpoint is exposed today.
- **akari is its own OAuth 2.1 authorization server**, so connecting an agent
  reuses the browser session rather than asking the user to mint and paste a token.
  The server publishes the protected-resource (RFC 9728) and authorization-server
  (RFC 8414) metadata, accepts dynamic client registration (RFC 7591), and runs the
  authorize and token endpoints. The authorize endpoint recognizes the `web_sessions`
  cookie a user already holds, so consent is one click; PKCE (S256) is mandatory,
  codes are single-use, and access tokens are short-lived with single-use refresh
  rotation. Tokens are minted read-only (`read` scope): an MCP credential can read
  everything a logged-in user sees and publish, delete, or mint nothing. New tables:
  `oauth_clients`, `oauth_auth_codes`, `oauth_tokens`, every secret stored only as
  its sha256, matching how API tokens, sessions, and invites are already handled. A
  user disconnects a client from the account page, revoking its tokens at once.

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

Built-in roots are optional because most machines do not run every supported
agent. Agent-provided overrides and configured extra roots are required: a
missing, malformed, inaccessible, or partially traversed required root is a
discovery error. A scan may return files from portions it completed, but the
one-shot command reports the incomplete scan and exits nonzero. Watch mode logs
the same error, deduped so a standing failure logs once (and at most once an
hour thereafter) rather than every discovery pass, and retries on later
discovery passes.

Discovery uses a closed symlink policy on every operating system. A matching
session-file symlink below a root is an error and is never followed, even when
its target is a regular file inside the root, and a plain directory symlink
below a root is ignored rather than descended into. A root itself that is a
symlink, or on Windows a directory junction (`mklink /J`; Go's `Lstat` does not
report a junction as a symlink, so it needs its own check, see
`discover.ClassifyRoot`), is rejected the same way by default, with one
exception: a linked built-in default root is skipped with a non-fatal notice
instead of an error, since those roots are already optional and a user who
junctioned their agent directory should not see sync start failing over it. Any
root, built-in or configured, can opt into following its own link with the
`follow_root_link` setting on an `extra_roots` entry; the no-follow policy still
governs everything found inside the walk regardless. Initial discovery, polling
metadata checks, filesystem events, and watch rescans all resolve a root through
the identical function, so they can never disagree about whether it is usable.

The closed root policy stops a link from redirecting discovery to an
unconfigured location, but it cannot by itself stop a *file* the walk already
approved from being swapped for a symlink in the moment between discovery and
the client actually reading it. Resolution's header peek closes that gap at
read time: it re-`Lstat`s the path immediately before opening it and rejects
anything but a regular file, then compares the opened file's own `Stat` against
that `Lstat` with `os.SameFile` before reading a single line, refusing to read
if the path was swapped for anything else in between. Discovery's closed policy
and this read-time identity check together are what keep a session's content
inside the location it was discovered under.

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
   treated the same as one with no remote. When the folder is a git work tree, the
   client also resolves `git -C <cwd> rev-parse --git-common-dir` to the repo root
   shared by every worktree (the local-root, see "Worktrees of a local-only
   repo") and sends it so the server can collapse the repo's worktrees; this is
   best effort and omitted when git cannot report it.
3. **Remote.** A single usable `origin` is canonicalized (see Projects); the
   result is the project key sent on ingest.

A remote session uploads with its canonical key. A standalone or orphaned session
uploads with its kind, its working directory, and (when it is a live worktree)
the shared repo root; the server derives the synthetic local key from machine
plus the root when present, else the working directory. The per-kind counts are surfaced (a periodic
summary in watch mode, a final tally in one-shot mode) so a user can see what is
backed up as standalone or orphaned. Only a file whose header cannot be read at
all is truly skipped, since there is then nothing to identify or send. Git is
invoked by shelling out to the system `git` with a short timeout; results are
cached per directory for the process lifetime.

### Upload

Drive the ingest protocol above, statelessly, once per file each time it is
visited:

- Announce the session, learn `stored_bytes` and the server's `prefix_sha256`.
- Verify: confirm the local file's first `stored_bytes` bytes hash to
  `prefix_sha256`, advancing the cached digest over only the newly stored bytes
  rather than re-hashing the whole prefix. On mismatch (or a local file shorter
  than `stored_bytes`), call reset and re-upload from zero; otherwise resume at
  `stored_bytes`.
- Stream the gap in boundary-aligned chunks (~1 MiB, growing to fit one oversized
  message up to the cap), scanning only newly appended bytes for the next
  boundary, advancing on each ack.

Streaming uploads have no total HTTP deadline because a healthy transfer of a
large tool body can take longer than any fixed request budget. Connection setup
remains bounded: dialing and the TLS handshake each have a 10-second timeout,
and response headers have a 30-second timeout. Once a body starts, the client
applies independent 60-second idle-progress windows to request writes and
response reads. Each window refreshes when bytes move and cancels the request if
the connection stalls. Caller cancellation still interrupts every phase. The
small JSON control requests (announce, existence checks, reset, and finalize)
keep a 60-second total deadline in addition to the idle window.

The client persists nothing to disk; its per-file cursor and digest live only in
memory. If the local file already matches the server (size equals `stored_bytes`,
hashes agree), the announce is the only call and no bytes move. Restarts, crashes,
and a fresh machine all recover by simply re-announcing, paying one re-hash to
rebuild the cache; divergence is always decided by the server's `prefix_sha256`.

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
- Log incomplete discovery passes with their error count. Files from complete
  portions still sync, and the next discovery pass retries failed roots or
  subtrees.

### One-shot mode

`akari sync` does a single discovery pass, uploads everything new since the
server's `stored_bytes` per file, prints a summary (uploaded, skipped with
reasons, and discovery errors), and exits. Any discovery error makes the exit
nonzero after safe files have been processed. This is the catch-up /
cron-friendly mode.

Discovered files sync in parallel, bounded by `--concurrency` (default
`min(NumCPU, 8)`). The cap stays modest on purpose: each file already fans its
own body uploads out under the client's shared adaptive limiter and CPU-bounded
compression encoder, so the file loop only needs enough parallelism to overlap
the per-file announce and existence-check round-trips. Outcomes are folded on one
goroutine that owns the running tally and the printed lines, so counts stay exact
and no two per-file lines interleave; the lines themselves now appear in
completion order rather than discovery order. The time limit and Ctrl-C keep their
meaning: once either fires, no new file is scheduled, but files already in flight
finish on a detached context. A second Ctrl-C exits the process outright.

### Daemon management

`akari watch` is the foreground loop. `akari daemon {start|stop|status}` manages
the same loop as a detached per-user process. A single advisory file lock ensures
only one client instance runs per machine. Its pidfile records both the PID and a
random per-run token; the token authenticates local control and distinguishes a
replacement process that reused the same PID.

`daemon stop` requests graceful shutdown over a user-only Unix-domain socket on
Unix or a random per-run named event on Windows. The watcher cancels its normal
run context, completes cleanup, and releases the advisory lock. The command does
not report success until it observes that release, so a successful stop is also
proof that another watcher can acquire the lock. Both the graceful wait and the
post-termination confirmation wait are bounded (10 seconds by default).

A timeout leaves the watcher running and returns an error. `--force` explicitly
permits escalation after the graceful path fails; immediately before terminating
the process, the command verifies that the lock is still held and the pidfile
still contains the instance it originally contacted. A changed identity fails
closed, so this recheck shrinks the window for PID reuse or a replacement
watcher to redirect the escalation down to the instant between validation and
signal delivery, which is as tight as portable APIs allow.

The detached client owns a size-rotating log writer rather than inheriting an
open append handle from its launcher. It closes the active handle before each
rename so rotation works on Windows, keeps the handoff serialized with writes,
and retains three 5 MiB history files beside the 5 MiB active log.

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

### Federated identity behind a trusted proxy

The default identity model is local: a username and an argon2id password per
account, invite-only after the first admin. To run akari inside an organization
that owns its own identity (a sidecar behind the org's gateway, so users are
created automatically and can be linked into from another app as the same user),
the server also accepts identity asserted by a trusted reverse proxy.

An authenticating proxy in front (oauth2-proxy, Pomerium, or the org's own
gateway) terminates auth against the org's IdP and forwards the authenticated
username in a request header. Akari trusts that header, provisions the account on
first sight, and treats the request as that signed-in user at full scope, the same
as a browser session. This is the standard "identity-aware proxy" pattern: the
proxy owns the session, and akari believes the header per request rather than
running its own login. Provisioned accounts are *federated*: they carry
`auth_source = 'proxy'` and no password, so the local `/login` form refuses them
(their only way in is the proxy). See `proxyPrincipal` in
`internal/server/httpapi/auth.go`.

Configuration (all off by default; a direct deployment sets none of these):

- `AKARI_PROXY_AUTH_HEADER`: the header the proxy sets to the authenticated
  username, e.g. `X-Auth-Request-Preferred-Username`. Setting it turns the mode
  on.
- `AKARI_PROXY_AUTH_SECRET`: an optional shared secret the proxy must echo for the
  identity header to be trusted. Defense in depth for when network isolation alone
  is not enough: a client that reaches akari directly cannot forge an identity
  without also knowing the secret.
- `AKARI_PROXY_AUTH_SECRET_HEADER`: the header carrying that secret (default
  `X-Akari-Proxy-Secret`), consulted only when the secret is set.

**Trust boundary.** Turning this on means akari believes anyone who can set the
identity header. That is safe only when akari is reachable *exclusively* through
the proxy that sets it (a private network, a sidecar on the same pod, an ingress
that always injects the header). Never expose a proxy-auth instance directly. The
shared secret hardens this but does not replace network isolation.

**Bootstrap the admin first.** A proxy-provisioned account is never admin, and
once any account exists, local registration is invite-only (which needs an
admin). So create the bootstrap admin through local password registration
*before* enabling proxy auth, otherwise the first proxied request creates a
non-admin account and there is no admin left to mint invites or run a reparse.

Two follow-on protocols extend this same federation seam and are tracked as
separate work: OIDC relying-party login with just-in-time provisioning (the
portable standard for orgs with their own IdP), and SCIM 2.0 for provisioning and
deprovisioning lifecycle. Both reuse the federated-account model introduced here
(nullable password, `auth_source`); OIDC and SCIM extend the `auth_source` CHECK
and add an external-subject identity when they land.

## Repository layout

```
cmd/
  akari/            # client binary
  akari-server/     # server binary (plus sweep, reparse, dev-seed subcommands)
internal/
  parser/           # claude, codex, pi parsers + normalized types (shared)
  casenc/           # client-side CAS body encoder (zstd policy, deterministic)
  gitremote/        # remote URL canonicalization
  pricing/          # compiled-in rate table + cost computation
  guide/            # embedded user-guide chapters + Markdown rendering
  devseed/          # dev-seed roster and ingest driver
  selfupdate/       # client self-update against GitHub releases
  shutdown/         # signal-driven shutdown context
  version/          # build version stamp
  server/
    httpapi/        # ingest + read handlers, OAuth, SSE
    mcpserver/      # MCP tools over the read surface
    ogimage/        # Open Graph preview card rendering
    web/            # templ templates, HTMX fragments, static assets
    store/          # postgres queries, CAS (large objects), migration runner
    storetest/      # per-test database provisioning
    auth/           # password, tokens, cookies
    parse/          # rebuild worker, parse epoch, fleet rebuild status
  client/
    discover/       # session file enumeration
    resolve/        # cwd -> git remote, skip-and-warn
    syncer/         # scheduling across sessions
    upload/         # ingest protocol driver (stateless)
    watch/          # fsnotify + polling fallback
    daemon/         # per-OS background management
  config/           # shared config loading
migrations/         # forward-only SQL, embedded into the server
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
2. **Parsing**: Claude, Codex, pi parsers; the rebuild-on-dirty parse worker;
   usage and cost; `reparse` command.
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
