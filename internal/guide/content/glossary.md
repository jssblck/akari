# Glossary

Definitions for the terms the rest of the guide uses.

## The session

A **session** is one agent run: a single, continuous interaction with Claude Code,
Codex, or pi, from the first message to the last. Each agent writes its session to
a log file on disk as it works, and that file is what akari ingests. A session
carries a stable **source id** (the agent's own identifier for the run), the
**machine** it ran on, the account that owns it, the git branch and working
directory it started in, timings, and its full transcript. A session still being
written streams to the server as it grows, so the UI can show it updating live.

Sessions are the unit you read, filter, price, and share. Everything else groups
or describes them.

## Projects

A **project** groups sessions by the code they were run against, keyed by
**canonical git remote**. When the client resolves a session, it reads the working
directory the session started in and asks git for that directory's `origin`
remote. The result is normalized to a `host/owner/repo` key (for example
`github.com/jssblck/akari`). Every worktree and every machine that clones the same
repository shares that remote, so their sessions collapse into one project
automatically. That is akari's core move: all the agent work on a repository in
one place, wherever it happened.

Not every session has a usable remote. The client classifies each into one of
three kinds:

- **Remote**: the working directory resolved to a canonical git remote. The
  project is that remote, shared across every machine and worktree.
- **Standalone**: the directory exists but yields no usable remote (it is not a
  git repository, has no `origin`, has several origins, or an origin URL akari
  does not recognize). The session is still kept, keyed to a synthetic local
  project identifying the machine and folder. A live worktree records its main
  worktree root so the server can still fold sibling worktrees together.
- **Orphaned**: the working directory is unknown or no longer exists on disk. The
  session is kept, keyed to its last-known local location.

Standalone and orphaned projects are grouped and labeled apart from git-remote
projects in the UI, so a folder that never had a remote does not masquerade as a
shared project. In the web UI they reach you through the Sessions filter rail
rather than the Projects index, which lists git-remote projects only.

## Machines and the fleet

A **machine** is a computer running the akari client. One machine can push
sessions from every agent installed on it; one server collects machines from a
whole team. Every session records the machine it came from, so you can filter a
project or the feed down to one workstation. The name defaults to the OS hostname
but is configurable (the `machine` config key or the `AKARI_MACHINE` environment
variable), so a fleet of ephemeral hosts can report under one stable identity
instead of a distinct one-off hostname per run. See
[the client](./the-client.md#machine-identity).

The **fleet** is everything a server holds: all projects, all sessions, all
machines. The Overview page reports fleet-wide, and the per-project analytics
report the same figures narrowed to one project. A **trailing window** (7, 30, or
90 days, a year, or all of history) bounds those rollups; every figure on a panel
respects the window you choose.

## The transcript

A session's **transcript** is its parsed contents: an ordered list of
**messages** (from the user, from the assistant, and system turns), each of which
may carry **thinking** (the model's internal reasoning, when the agent records
it), a **model** id, one or more **tool calls**, and **attachments** (images and
other files that rode along with a message). A tool call records the tool's name,
an optional category and file path, and its **input** and **result** bodies, each
with a size, a media type, and a status.

Some sessions spawn **subagents**: child sessions launched from within a parent
run. akari links them to the session that spawned them, so a subagent shows up
under its parent rather than floating loose in the feed.

Tool bodies can be large (a single result might be megabytes of JSON or a
base64-encoded image), so the transcript does not inline them. It holds a small
reference, and the body itself lives in content-addressed storage (below), fetched
only when you expand it.

## Tokens and cost

Every turn records its token usage, split into classes akari tracks separately:

- **Input** tokens fed to the model.
- **Output** tokens the model generated.
- **Cache write** tokens written to the model's prompt cache.
- **Cache read** tokens served from that cache.

The server prices each session from a **compiled-in rate table**: a mapping from
model to per-token rates, built into the binary. Parsing a session looks up its
model and multiplies each token class by its rate to get a dollar cost. There is
no runtime pricing feed; updating rates means a new server build (which, because
pricing is part of parsing, reprices old sessions automatically on the next
[reparse](#parsing-and-reparse)).

When a session uses a model the table does not know, its tokens are still recorded
but that portion of the cost is left unpriced, and the session is marked
**cost incomplete**. The UI shows such a total with a trailing `+` (for example
`$1.42+`), so a partial figure is never silently rounded to look complete. Costs
below a cent show extra precision rather than collapsing to `$0`.

Per-session totals roll up across the session's turns; fleet and project totals
roll those up further, always within the selected trailing window.

## Content-addressed storage

Bulky tool bodies live in a **content-addressed store** (CAS): each body is keyed
by the SHA-256 hash of its bytes and stored once, deduplicated across every
session that references it. The transcript keeps only the hash and the body's
size and media type; the UI shows a compact chip ("36 KB json") that fetches the
real bytes on demand. Storing a body once no matter how many sessions repeat it
keeps the database from ballooning on near-identical tool output.

CAS bytes are served **per session**, never by bare hash. You can fetch a body
only through a session that references it and that you are allowed to see, so the
cross-session deduplication never leaks an internal body out through a public
link.

## Parsing and reparse

The server keeps two things for each session: the **raw bytes** it was ingested
from, stored losslessly, and a **projection** parsed out of them (the messages,
tool calls, usage, and cost the UI reads). The raw bytes are the source of truth;
the projection is derived and disposable.

That split is what makes **reparse** possible. A **reparse** replays stored raw
bytes through the current parser and rebuilds the projection from scratch. It is
how a parser or pricing improvement reaches sessions already ingested, with
nothing re-uploaded. Reparse runs three ways:

- **Automatically on startup.** The parser carries a compiled-in **epoch**; the
  server compares it against the epoch the stored data was last built under and,
  when they differ, reparses in the background while it keeps serving. There is no
  manual step after a parser upgrade.
- **From the admin UI.** An admin can force a reparse from the Account page and
  watch its progress.
- **From the CLI.** `akari-server reparse` forces one regardless of epoch, and
  `--agent <name>` limits it to one agent's sessions.

While a reparse rebuilds, the parsed pages show a "reparse in progress" notice
with a live progress bar rather than serving a half-rebuilt projection. Raw-byte
reads (and content-addressed bodies) stay available throughout, since they are not
part of the projection being rebuilt. [Self-hosting](./self-hosting.md#reparse)
covers running one.

## Visibility

A session's **visibility** is `internal` by default: visible to any signed-in
user of the server. There is no private-to-one-user state; on a given server,
signed in means you see everything. To share a session publicly, its owner
**publishes** it, minting an unguessable public link at `/s/<public-id>` that a
logged-out viewer can open; unpublishing clears the link. A user can likewise
publish their own **usage overview** at `/u/<username>`. The full model, including
who can delete what, is [Accounts and sharing](./accounts-and-sharing.md).

---

Back to the [overview](./index.md) for the map, or the
[akari repository](https://github.com/jssblck/akari) for the engineering design
and code.
