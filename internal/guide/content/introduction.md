# Introduction

## The problem

Claude Code, Codex, and pi each write a detailed session log to disk as they
work: every message, every piece of thinking, every tool call and its result, the
tokens spent. That log is the record of what an agent actually did, and it is
worth keeping. But by default it lives in a dot-directory on one laptop, in a
format built for the tool that wrote it, with no cost attached and no way to
search across the run you half-remember from last week on a different machine.

Three things follow from that:

- **The record is scattered.** The same repository, worked on from two worktrees
  or two machines, leaves its sessions in two places that never meet. There is no
  one history to search.
- **The record is ephemeral.** A reinstall, a cleared cache, or a new machine and
  the trail is gone. Nothing is backed up.
- **The spend is invisible.** The logs carry token counts but not dollars, and no
  rollup tells you where the money and the tokens are going across projects and
  over time.

## The core idea

akari is an explicit **client/server split**. Many thin **clients** push raw
session bytes to one **server**; the server does all the parsing, storage, and
rendering. Three properties define the split:

1. **Raw bytes in, parsing on the server.** The client does not interpret the
   logs. It discovers them, resolves each to a git project, and streams the
   unmodified bytes. The server is the one place that parses, prices, and stores.
   Because the client keeps no derived state, a better parser reaches every old
   session by re-parsing the bytes already on the server, with nothing
   re-uploaded. That rebuild is automatic: a new server binary notices its parser
   changed and rebuilds in the background.
2. **Keyed by git remote.** A session is filed under the canonical git remote of
   the directory it ran in. The same repository cloned into several worktrees, or
   onto several machines, resolves to the same remote and collapses into one
   **project**. You see all the agent work on a repository in one place, no matter
   where it happened. A session with no usable remote is still kept, keyed to its
   local folder instead.
3. **One shared history.** Everyone signed in to a server sees every session on
   it, with no per-user walls. Sharing a session **publicly** is a deliberate act:
   you publish it, minting an unguessable link a logged-out viewer can open.

Everything else (the cost ledger, the live transcript view, the MCP endpoint an
agent reads) is built on those three.

## What akari is and is not

What it is:

- **A backup and a ledger.** Sessions are stored losslessly and priced, so the
  record survives a wiped laptop and you can see where tokens and cost go.
- **A reading surface for humans and agents alike.** The same history is a web UI
  you browse and a read-only [MCP](./agent-access.md) endpoint an agent queries.

What it is not:

- **Not partitioned per user.** Everyone signed in to a server sees every session
  on it. The history is private to that team, not to the individual who ran each
  session; there are no per-user walls to manage. To reach someone without an
  account, you publish a session.
- **Not multi-tenant.** One server is one team's shared history. Isolation between
  teams is one server per team, not per-user partitions inside one.
- **Not an agent runner.** akari never runs an agent or writes to your code. It
  reads the logs your agents already produce. Its MCP surface is read-only by
  construction.

The practical shape of sharing and access is [Accounts and
sharing](./accounts-and-sharing.md).

---

Next: [Getting started](./getting-started.md) -> install the client and push your
first sessions.
