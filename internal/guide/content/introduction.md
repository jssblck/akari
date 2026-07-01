# Introduction

> Why akari exists, and the one idea to hold in your head.

## The problem

Coding agents leave a trail, and then it evaporates. Claude Code, Codex, and pi
each write a detailed session log to disk as they work: every message, every
piece of thinking, every tool call and its result, the tokens spent. That log is
the record of what an agent actually did, and it is worth keeping. But by default
it lives in a dot-directory on one laptop, in a format built for the tool that
wrote it, with no cost attached and no way to search across the run you
half-remember from last week on a different machine.

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
rendering. Hold three properties in your head and the rest of the system follows:

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
   it. akari is a team's shared instrument, not a private vault with per-user
   walls. Sharing outward is a deliberate act: you **publish** a session to mint
   an unguessable link a logged-out viewer can open.

Everything else (the cost ledger, the live transcript view, the MCP endpoint an
agent reads) is built on those three.

## The mental model

Picture akari as an instrument panel wired to your whole fleet of agents.

> Each machine has a small sensor (the client) that notices when an agent writes
> to its session log and streams the new bytes to a central recorder (the
> server). The recorder transcribes them into a common format, tags each with the
> project it belongs to, prices the tokens, and puts the result on a set of gauges
> you read: a fleet-wide overview, a feed of every session, a page per project,
> and the full transcript of any single run.

The data is the product; the interface stays out of its way. Figures are exact
and stable, a cost the rate table could not fully price shows as partial rather
than complete, and a run in progress updates live as its bytes arrive. You read
akari; you do not configure it.

## What akari is and is not

Two things it is, stated plainly:

- **A backup and a ledger.** Sessions are stored losslessly and priced, so the
  record survives a wiped laptop and you can see where tokens and cost go.
- **A reading surface for humans and agents alike.** The same history is a web UI
  you browse and a read-only [MCP](./agent-access.md) endpoint an agent queries.

And what it is not:

- **Not a private vault.** There is no private-to-one-user session. Signed in
  means you see everything on the server; that is the design, not a gap. If a
  session should reach someone without an account, you publish it.
- **Not multi-tenant.** One server is one team's shared history. Isolation between
  teams is one server per team, not per-user partitions inside one.
- **Not an agent runner.** akari never runs an agent or writes to your code. It
  reads the logs your agents already produce. Its MCP surface is read-only by
  construction.

Those boundaries are why the model stays simple. The practical shape of sharing
and access, within them, is [Accounts and sharing](./accounts-and-sharing.md).

---

Next: [Getting started](./getting-started.md) -> install the client and push your
first sessions.
