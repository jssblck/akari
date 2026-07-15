# Getting started

This chapter gets you from nothing to a session history flowing to a server. It
assumes a server already exists (a teammate runs one, or you do). If you need to
stand one up first, [Self-hosting](./self-hosting.md) does it with a single
`docker compose up`, then come back here.

Terms like *session*, *project*, *ingest token*, and *scope* appear in passing;
the [Glossary](./glossary.md) defines each.

## 1. Install the client

The `akari` client runs on macOS, Windows, and Linux. The quickest path is the
install script: it downloads the release archive for your platform, verifies it
against the release `SHA256SUMS`, and puts `akari` on your `PATH`.

On macOS and Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/jssblck/akari/main/scripts/install.sh | sh
akari version
```

On Windows, from PowerShell:

```powershell
irm https://raw.githubusercontent.com/jssblck/akari/main/scripts/install.ps1 | iex
akari version
```

Set `AKARI_VERSION` (for example `v0.1.0`) to pin a release instead of taking the
latest. Prefer to build from source? With a Go toolchain:

```sh
go build -o akari ./cmd/akari
./akari version
```

The client updates itself in place later with `akari update` (and `akari update
--check` reports whether one is available without installing it).

## 2. Point the client at your server

The client authenticates to the server with an **ingest token**: a push-only
credential scoped so it can upload sessions and nothing else. It cannot read your
history or mint other tokens.

Mint one from the server's web UI:

1. Sign in to the server in a browser.
2. Open **Account**, find **API tokens**, and create a token with the **ingest**
   scope. (The three scopes are `ingest`, `read`, and `full`; see
   [Accounts and sharing](./accounts-and-sharing.md#api-tokens).)
3. Copy the token. It is shown once.

Then hand the server URL and token to `akari login`:

```sh
akari login --server https://akari.example.com --token <ingest-token>
```

This writes a small config file (server URL and token only) to your OS config
directory, with owner-only permissions. That is the client's entire persistent
state; it keeps no session bookkeeping of its own. [The client](./the-client.md#configuration)
covers the config file and its options in full.

## 3. Push your sessions

Do a dry run first to see what the client found and where each session would be
filed, without uploading anything:

```sh
akari sync --dry-run
```

Each discovered session prints its resolved project (a git remote, or a local
folder when the working directory is not a git repository) or its skip reason.
When it looks right, push for real:

```sh
akari sync
```

`akari sync` discovers the session logs Claude Code, Codex, and pi leave in their
standard locations, resolves each to its git project, and streams the new bytes to
the server in one pass, then exits. Uploads resume from the server's cursor, so a
re-run only sends what is new.

By default `sync` stops starting new uploads after five minutes (the file it is on
when the limit hits still finishes cleanly); tune it with `--time-limit`, a Go
duration such as `30s` or `10m`, or `0` to remove the cap:

```sh
akari sync --time-limit 30s     # grab a quick sample, then stop
```

## 4. Keep it flowing

`sync` is one-shot. To keep pushing as your agents work, run the watcher, which
uploads sessions as they change:

```sh
akari watch                # foreground; Ctrl-C to stop
```

Or run the same loop in the background and manage it as a per-user daemon:

```sh
akari daemon start         # launch watch in the background; prints its PID and log path
akari daemon status        # is it running?
akari daemon stop          # stop it and confirm the single-instance lock is free
```

Run `akari daemon start` once and the watcher keeps uploading in the background.
`daemon stop` waits up to 10 seconds for graceful cleanup. If it times out, it
leaves the watcher running and exits non-zero; use `--timeout <duration>` to wait
longer or `--force` to permit termination after the graceful attempt.

## 5. Read what you pushed

Open the server in a browser. Your sessions appear on:

- **Overview**: fleet-wide cost, tokens, and session counts for a trailing
  window, with an activity heatmap.
- **Sessions**: every session in one feed, filterable by agent, project, user,
  and machine.
- **Projects**: repositories and local folders with lifetime totals and a
  30-day token trend.

Click any session to read its full transcript. [The web UI](./the-web-ui.md) is
the tour. To read the same history from a coding agent instead of a browser, wire
up the [MCP endpoint](./agent-access.md).

## When something looks off

The most common first-run snags:

- **`akari sync` uploaded nothing.** Run `akari sync --dry-run` and read the skip
  reasons. A session whose working directory is not a git repository is filed
  under a local folder rather than skipped; a session file the client cannot read
  a header from is skipped entirely.
- **A session went to a "local" project you did not expect.** Its working
  directory had no usable git `origin` remote (not a repo, no `origin`, several
  origins, or an unrecognized origin URL), so it was keyed to the machine and
  folder instead of a shared project. See
  [Glossary](./glossary.md#projects).
- **`akari login` succeeds but `sync` is rejected.** The token is probably not an
  ingest (or full) scope token, or it was revoked. Mint a fresh **ingest** token
  and log in again.
- **The client cannot find your sessions.** akari looks in each agent's standard
  location. If yours live elsewhere, add an `extra_roots` entry to the config;
  see [The client](./the-client.md#discovery).

---

Next: [The client](./the-client.md) -> the CLI in depth: how it discovers,
resolves, and uploads.
