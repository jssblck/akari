# The client

The `akari` client is the piece that runs on each machine. It finds the session
logs your agents write, works out which git project each belongs to, and streams
the raw bytes to the server. It keeps almost no state of its own: a config file
with a server URL and a token, and nothing else that has to survive a restart.
This chapter is the reference for driving it.

## Commands

| Command | What it does |
| --- | --- |
| `akari login --server <url> --token <token>` | Write the client config (server URL and token). |
| `akari sync` | Discover and upload everything new, then exit. |
| `akari watch` | Stay running and upload sessions as they change (foreground). |
| `akari daemon start` \| `status` \| `stop` | Run and manage `watch` as a background process. |
| `akari update` | Update the client to the latest release in place. |
| `akari version` | Print the build version and exit. |

Every command takes `--config <path>` to point at a config file other than the
default. A first `Ctrl-C` stops `sync` from starting new files (the one in flight
finishes) and winds `watch` down gracefully; a second exits at once.

### login

```sh
akari login --server https://akari.example.com --token <token>
```

`login` writes the server URL and API token to the config file and exits. The
token is minted out of band on the server (its account page) and passed in;
`login` does not create it. Use an **ingest**-scope token for a push-only client,
or a **full**-scope token if the same credential also drives the web API. It
preserves any `extra_roots` and `excludes` already in the config, so re-running it
to rotate a token or move servers does not wipe your discovery settings.

### sync

```sh
akari sync [--dry-run] [--time-limit <dur>] [--concurrency <n>]
```

`sync` makes one pass: discover every session file, resolve each to a project,
upload what is new, and exit. Because uploads resume from the server's cursor, a
re-run sends only bytes the server does not already have.

- `--dry-run` resolves and reports what would upload, with a skip reason for each
  file it would not, and uploads nothing. Run it first when setting up a machine.
- `--time-limit <dur>` caps how long `sync` keeps *starting* new uploads, a Go
  duration such as `30s` or `5m` (default `5m`; `0` removes the cap). The limit
  gates only when new work begins, so the file being uploaded when the limit
  elapses runs to a clean stopping point rather than being abandoned mid-stream.
  Because uploads resume, repeated short runs ingest a backlog in chunks.
- `--concurrency <n>` bounds how many files upload in parallel (default the CPU
  count, capped at 8). Each file also parallelizes its own body uploads under a
  shared limiter, so the file-level cap stays modest on purpose. A given file
  never races with itself.

### watch

```sh
akari watch
```

`watch` runs in the foreground and uploads sessions as they change, logging to
standard error. It holds a single-instance lock for its lifetime, so two watchers
cannot run at once. Under the hood it layers three change detectors so nothing is
missed: an OS file-system watcher for prompt, debounced uploads; a periodic
re-stat of known files to catch changes the OS watcher drops (network filesystems,
watch exhaustion); and a slower full rescan that rediscovers roots for
newly created files. It does an initial full pass before entering the event loop,
ingesting any backlog on startup.

### daemon

```sh
akari daemon start     # launch watch in the background; prints its PID and log path
akari daemon status    # report whether it is running, and its PID
akari daemon stop      # stop the running watcher
```

`daemon` runs the same `watch` loop as a detached, per-user background process (it
is not a system service). It writes a pidfile and a log file under your config
directory; `start` confirms the child took the single-instance lock before
returning, and `stop` verifies a live instance holds it before signaling. This is
the steady state on a workstation: run `akari daemon start` once.

### update and version

```sh
akari update            # update to the latest release in place
akari update --check    # report whether an update is available, install nothing
akari update --force    # reinstall the latest even if already current
akari version           # print the build version
```

`update` is a native updater: it downloads the latest release archive for your
platform, verifies it against the release `SHA256SUMS`, and swaps the binary in
place, with no shell or `curl` involved. On Windows it moves the running
executable aside so the update succeeds while akari is running; restart any
`akari watch` or `akari daemon` afterward to pick up the new version.

## Configuration

`akari login` writes the config; you can also edit it by hand. It is a TOML file
in your OS config directory:

| Platform | Path |
| --- | --- |
| macOS | `~/Library/Application Support/akari/config.toml` |
| Linux | `~/.config/akari/config.toml` |
| Windows | `%AppData%\akari\config.toml` |

Pass `--config <path>` to any command to use a different file. A full example:

```toml
server_url = "https://akari.example.com"
token      = "akari_ingest_..."

# Discover sessions from extra locations, beyond each agent's standard root.
[[extra_roots]]
agent = "claude"
path  = "/mnt/shared/claude-sessions"

# Skip paths matching these globs during discovery, for sync and watch alike.
excludes = ["**/scratch/**", "*.private.jsonl"]
```

The keys:

- **`server_url`** (required): the server's base URL, no trailing slash.
- **`token`** (required): the API token, used as a bearer credential. Ingest scope
  is the right choice for a push-only client. The file is written with owner-only
  permissions, since it holds a credential.
- **`extra_roots`** (optional): additional discovery roots, each an
  `{ agent, path }` pair where `agent` is `claude`, `codex`, or `pi`. Use these
  when your sessions live somewhere other than the standard location.
- **`excludes`** (optional): glob patterns of paths to skip, applied to both
  `sync` and `watch`. Patterns match the full path with `/` separators;
  `**/scratch/**` ignores any path with a `scratch` segment, `*.private.jsonl`
  excludes by suffix. Empty discovers everything.

The client defines no environment variables of its own, but it honors each
agent's own root override (see below).

## Discovery

On every run the client looks for session files in each agent's standard location,
plus any `extra_roots` you configured:

| Agent | Default root | Override |
| --- | --- | --- |
| Claude Code | `~/.claude/projects` | `CLAUDE_PROJECTS_DIR` |
| Codex | `~/.codex/sessions` and `~/.codex/archived_sessions` | `CODEX_SESSIONS_DIR` |
| pi | `~/.pi/agent/sessions` | `PI_DIR` (sessions at `$PI_DIR/agent/sessions`) |

Missing roots and excluded paths are skipped without error. For each candidate
file, the client peeks the first line to read the working directory and session
id, then resolves that directory's git `origin` remote to a project key. A file
whose header cannot be read is skipped entirely; a directory with no usable remote
produces a standalone or orphaned project rather than being dropped
([Glossary](./glossary.md#projects)).

## How the upload works

The upload protocol is not something you invoke directly, but its shape explains
the client's behavior. The upload is **resumable** and **append-only**, and the
client is stateless across runs:

1. **Announce.** The client tells the server about a file (its agent, source id,
   project, branch, machine). The server replies with how many bytes it already
   holds for that session and a digest of that verified prefix.
2. **Reconcile.** The client hashes its local file up to the server's cursor. If
   the digest matches, the prefix is verified and the client resumes from there.
   If it does not (the file was truncated, rewritten, or rotated), the client
   resets the session and re-uploads from the start.
3. **Stream.** The client streams the remaining bytes as newline-aligned chunks.
   As it goes, it lifts large tool bodies out of the transcript into
   content-addressed storage: it checks whether the server already holds each body
   by hash, uploads the ones it does not, and leaves a small reference in the
   stream. The server parses each chunk as it lands, so the projection fills in
   incrementally and a live session appears to grow in the UI.

Two consequences worth knowing:

- **A session's final turn is withheld until its file goes idle.** The last turn
  often has no closing line to mark it complete, so the client waits for the file
  to be untouched briefly before flushing it.
- **Re-running is cheap and safe.** Because the server tracks the cursor and the
  client re-derives everything from the file, `sync` after `sync` uploads only new
  bytes, and an interrupted upload resumes rather than restarting.

---

Next: [The web UI](./the-web-ui.md) -> reading the history you just pushed.
