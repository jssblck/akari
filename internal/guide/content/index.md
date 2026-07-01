# akari user guide

> One searchable history of every AI coding-agent session across your fleet, self-hosted.

This guide teaches you how to run akari for yourself or your team: what it is, how
to push your coding-agent sessions to it, how to read them back, and how to host
the server. It is written for two readers at once, the person browsing their
history in the web UI and the coding agent reading it over MCP, because akari
serves the same data to both.

The guide is self-contained: everything you need to install the client, ingest
sessions, share them, and stand up a server is here. If you want to work on akari
itself rather than use it, the engineering design and code live in the
[akari repository](https://github.com/jssblck/akari).

> **Reading this as an agent?** The whole guide is also served as a single
> plain-text file at [`/llms-full.txt`](/llms-full.txt), so you can ingest every
> chapter in one fetch instead of crawling pages. A machine-readable index of the
> chapters is at [`/llms.txt`](/llms.txt), and any page is available as raw
> Markdown by appending `.md` to its URL.

## Read in order

The chapters build on each other. Read them top to bottom and you go from "what
is this" to "running the server with agents reading it" without backtracking.

1. **[Introduction](./introduction.md)**: the problem akari solves, the one idea
   to hold in your head (raw bytes in, parsing on the server, keyed by git
   remote), and what akari is and is not. Start here.
2. **[Getting started](./getting-started.md)**: install the client, mint an
   ingest token, and push your first sessions in about five minutes.
3. **[Concepts](./concepts.md)**: sessions, projects, machines, the fleet,
   transcripts, tokens and cost, content-addressed storage, and reparse. The
   vocabulary the rest of the guide assumes.
4. **[The client](./the-client.md)**: the `akari` CLI in depth. `login`, `sync`,
   `watch`, and the `daemon`; how it discovers sessions on disk; and the
   resumable, append-only upload.
5. **[The web UI](./the-web-ui.md)**: reading your history. The overview and its
   trailing windows, the session feed, projects, and the transcript view with its
   tool bodies and live updates.
6. **[Accounts and sharing](./accounts-and-sharing.md)**: registration and
   invites, the three token scopes (`ingest`, `read`, `full`), session visibility,
   and publishing a session or your usage overview.
7. **[Agent access](./agent-access.md)**: point a coding agent at your history
   through the read-only Model Context Protocol endpoint. The connect flow and the
   full tool catalog.
8. **[Self-hosting](./self-hosting.md)**: run the server. Docker Compose,
   configuration, the database, the first admin account, and reparse.

## In a hurry: get your sessions flowing

If your goal is "get my agent sessions into an akari server my team already
runs," here is the whole path; each step links to its detail:

1. **Install the client.** [Getting started](./getting-started.md#1-install-the-client).
2. **Mint an ingest token** on the server's account page and run
   `akari login --server <url> --token <token>`.
   [Getting started](./getting-started.md#2-point-the-client-at-your-server).
3. **Push once, then keep pushing.** `akari sync` uploads everything new; `akari
   watch` (or `akari daemon start`) keeps it flowing.
   [Getting started](./getting-started.md#3-push-your-sessions).
4. **Read them.** Open the server in a browser, or connect a coding agent over
   [MCP](./agent-access.md).

No server yet? [Self-hosting](./self-hosting.md) stands one up with a single
`docker compose up`.

## The one-paragraph version

You run one **server** (backed by Postgres) and point many thin **clients** at
it, one per machine. The client discovers the session logs Claude Code, Codex,
and pi leave on disk, resolves each session's working directory to a canonical
git remote, and streams the raw bytes to the server with a resumable, append-only
protocol. The server stores those bytes losslessly, parses them into a normalized
projection (messages, tool calls, token usage, cost from a compiled-in rate
table), and serves a web UI and a read-only [MCP](./agent-access.md) endpoint over
it. **Projects** are keyed by git remote, so the same repository across worktrees
and machines collapses into one. Because the client keeps no derived state, a
parser improvement reaches old sessions by re-parsing on the server, with nothing
re-uploaded. Everyone signed in sees every session; you **publish** one to share
it with a logged-out viewer.

## Good to know

A few constraints shape everything that follows:

- **The client runs anywhere; the server is Linux-only.** Push from macOS,
  Windows, or Linux; host the server on Linux (a container or a systemd service).
- **Supported agents are Claude Code, Codex, and pi.** The client reads the
  session logs each leaves in its standard location.
- **Authorization is deliberately flat.** Signed in means you see every session.
  There is no private-to-one-user state; sharing is a matter of publishing, not of
  per-user walls. [Accounts and sharing](./accounts-and-sharing.md) covers the
  full model.
- **A session that did not run in a git repository is still kept**, but keyed to a
  local folder rather than a shared project. [Concepts](./concepts.md#projects)
  explains how that resolution works.

---

Next: [Introduction](./introduction.md) -> the problem akari solves and the one
idea to hold in your head.
