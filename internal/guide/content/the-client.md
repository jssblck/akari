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
akari login --server https://akari.example.com --token <token> [--machine <name>]
```

`login` writes the server URL and API token to the config file and exits. The
token is minted out of band on the server (its account page) and passed in;
`login` does not create it. Use an **ingest**-scope token for a push-only client,
or a **full**-scope token if the same credential also drives the web API. It
preserves any `extra_roots` and `excludes` already in the config, so re-running it
to rotate a token or move servers does not wipe your discovery settings.

`--machine <name>` sets the logical machine name this client reports for every
session (see the `machine` config key below). Omit it to keep the OS hostname, or
to leave an existing name untouched on a re-run; pass `--machine ""` to clear it
back to the hostname.

### sync

```sh
akari sync [--dry-run] [--time-limit <dur>] [--concurrency <n>] [--finalize]
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
- `--finalize` treats every session as terminal, flushing each one's final turn
  now instead of waiting for the file to go idle (see "How the upload works"
  below). It also tells the server the session is finished: the announce marks it
  terminal and, once the whole transcript has landed, the client asks the server
  to grade it immediately rather than waiting out the server-side settle window
  (30 minutes idle). So on an ephemeral host the quality grade is available at the
  end of the run, in time to report or gate on, instead of long after the host is
  gone. Use it on a host that disappears right after the sync, a CI job or a cloud
  sandbox, where neither wait would elapse and the last turn would otherwise never
  upload and the grade would never land. Reach for it only when every session is
  genuinely finished: on a workstation where a session may still be running, it
  would flush a turn mid-stream, so let the idle wait do its job there instead.

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
akari daemon stop      # request shutdown; wait up to 10s for cleanup and lock release
akari daemon stop --timeout 30s   # allow longer for in-flight uploads
akari daemon stop --force         # escalate only if graceful shutdown fails
```

`daemon` runs the same `watch` loop as a detached, per-user background process (it
is not a system service). It writes a pidfile and `akari.log` under your config
directory; `start` confirms the child took the single-instance lock before
returning. `stop` sends an authenticated local shutdown request, then waits until
the watcher exits and releases that lock. A zero exit status therefore means a
new watcher can start immediately. Unix uses a user-only Unix-domain socket for
the request; Windows uses a random per-run named event, so it follows the same
cleanup path instead of being killed.

The default timeout is 10 seconds. If cleanup does not finish, `stop` exits
non-zero and leaves the watcher running. `--timeout <duration>` changes the bound.
`--force` keeps the graceful request as the first step, then terminates the
recorded process after the timeout and waits again for lock release. Before that
escalation, `stop` re-reads the per-run identity in the locked pidfile; if another
watcher has replaced it, the command fails instead of targeting the new process.
A forced, confirmed stop exits zero and prints `akari watch force-stopped`, which
distinguishes it from ordinary cleanup and from a timeout.

The pidfile now contains a JSON process identity instead of a bare PID. A client
upgraded while an older daemon is still running cannot safely authenticate or
escalate against that old process. Stop the daemon with the old client before
upgrading, or end that process through the operating system once; the next
`daemon start` writes the new identity format.

The log rotates while the daemon runs: each file is capped at 5 MiB, three rotated files
(`akari.log.1` through `akari.log.3`) are retained, and the whole set is bounded
at 20 MiB. The active and rotated files remain owner-only. This is the steady
state on a workstation: run `akari daemon start` once.

| Platform | Daemon log |
| --- | --- |
| macOS | `~/Library/Application Support/akari/akari.log` |
| Linux | `~/.config/akari/akari.log` |
| Windows | `%AppData%\akari\akari.log` |

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

# Report every session under one stable machine name instead of the hostname.
machine = "sandbox-pool"

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
- **`machine`** (optional): the logical machine name reported for every session
  this client uploads. Empty falls back to the OS hostname. Set it to give a fleet
  of ephemeral or containerized hosts (CI jobs, autoscaled workers, throwaway dev
  containers) one stable identity such as `ci` or `sandbox-pool`, so the machine
  filter does not fill with thousands of single-use hostnames. `AKARI_MACHINE`
  overrides it per run.
- **`extra_roots`** (optional): additional discovery roots, each an
  `{ agent, path }` pair where `agent` is `claude`, `codex`, or `pi`. Use these
  when your sessions live somewhere other than the standard location.
- **`excludes`** (optional): glob patterns of paths to skip, applied to both
  `sync` and `watch`. Patterns match the full path with `/` separators;
  `**/scratch/**` ignores any path with a `scratch` segment, `*.private.jsonl`
  excludes by suffix. Empty discovers everything.

## Machine identity

Every session records the **machine** it came from, a dimension you can filter the
feed and each project by. By default that is the OS hostname. On a workstation
that is exactly what you want; on ephemeral or containerized hosts it is not,
because each run gets a distinct one-off hostname and the machine filter fills with
thousands of single-use values that mean nothing.

Give such a fleet one stable logical machine instead. The name is resolved from
three sources, highest priority first:

1. the **`AKARI_MACHINE`** environment variable, a per-run override that needs no
   config file (the easy path for a container that sets env far more readily than
   it writes config);
2. the **`machine`** config key, set at `akari login --machine <name>` or by hand,
   for a stable per-host or per-fleet name;
3. the **OS hostname**, the default when neither is set.

The sessions still aggregate by project, user, and agent exactly as before; only
the machine label changes, so an ephemeral fleet reporting as `ci` shows up as one
machine rather than polluting the rail.

`AKARI_MACHINE` is the only environment variable akari itself defines. The client
also honors each agent's own root override for discovery (see below).

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
   stream. The server re-parses the session moments after chunks land, so a live
   session appears to grow in the UI as it runs.

Two consequences worth knowing:

- **A session's final turn is withheld until its file goes idle.** The last turn
  often has no closing line to mark it complete, so the client waits for the file
  to be untouched briefly before flushing it. On a host that is torn down right
  after the sync (CI, a cloud sandbox), that idle wait never elapses, so pass
  `akari sync --finalize` to flush the final turns immediately. `--finalize` also
  marks each session terminal on the server so its quality grade is computed at the
  end of the run rather than after the server's own 30-minute settle window, which
  the host would not be around to see.
- **Re-running is cheap and safe.** Because the server tracks the cursor and the
  client re-derives everything from the file, `sync` after `sync` uploads only new
  bytes, and an interrupted upload resumes rather than restarting.

---

Next: [The web UI](./the-web-ui.md) -> reading the history you just pushed.
