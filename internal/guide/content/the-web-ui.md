# The web UI

The web UI is where a human reads what the agents did. It is server-rendered: a
persistent left sidebar carries the primary sections (Overview, Insights,
Projects, Sessions, Account), with the signed-in user and a log-out control at
its foot. Reading the UI needs a full-scope credential, which in practice is a
browser session; signing in gives you that. Every timestamp renders in your own
timezone: the browser reports its zone in a cookie once, and the server formats
against it from the next page on.

## Overview

**Overview** is the landing surface: fleet-wide usage bounded to a trailing
window. Pick the window (7, 30, or 90 days, a year, or all of history) and every
figure on the panel follows it:

- **Cost, combined tokens, and session totals** for the window, as stable tabular
  figures. A partial cost (some session used a model the rate table does not
  price) shows a trailing `+`.
- **A daily-activity heatmap**, one cell per day, so a busy stretch or a quiet one
  is visible at a glance.
- **By-model and by-agent breakdowns** of where the usage went, plus a by-user
  breakdown once more than one account has usage in the window.

You can also scope the overview to specific accounts.

## Insights

**Insights** reads how the window's sessions went, where the Overview reads what
they cost. The same trailing-window selector bounds every panel, and the toolbar
shows how many sessions the window holds. Top to bottom:


- **Concurrency and velocity**: how many sessions ran at once at the fleet's peak
  (and when), the busiest single user, and the average; then how fast turns
  cycled: the median and slow-tail (p90) response latency, the opening reply on
  its own, and messages and tool calls per active minute.
- **Tools**: total calls, the fleet-wide error rate, and calls per turn, over a
  list of the busiest tools, each bar sized by call volume and colored by its own
  reliability.
- **Prompt hygiene**: how clearly the window's prompts set the agent up: the
  share that were terse, repeated, or carried no code pointer, and the sessions
  that opened unstructured. These rates describe the human's input and never move
  a grade.
- **Context health**: how heavy sessions got (the median, p90, and heaviest peak
  context, as raw token counts) and how often they shed context (the share of
  sessions with at least one inferred reset, and the total). A load measure, not
  spend.
- **People**: one row per author, busiest first: session count, an outcome-mix
  bar (hover it for the counts), how many of their sessions carry a grade, and
  their average score. A name links to that author's sessions. The table appears
  only when two or more authors ran sessions in the window; a single author would
  just restate the fleet figures.
- **Grades**: the quality score banded A to F, with an **unscored** bucket for
  sessions that carry no grade. A note on the panel says what share of the window
  is graded (for example "62% graded"), so you can weigh the distribution by how
  much it speaks for.
- **Outcomes**: how each session ended. **Completed** means the agent had the
  last substantive word with nothing left hanging; **abandoned** means the human
  walked away without a reply or interrupted a tool; **errored** means it stopped
  on failing tool calls or died mid-tool with no human in the loop; **unknown**
  means there is no verdict yet: the session is still live, or there was nothing
  to read.
- **Archetypes**: what kind of session each was, by length and turn count.
  **Quick** is a short exchange, **standard** an ordinary working session,
  **deep** a long and involved one, **marathon** an exceptionally long or
  message-heavy one, and **automation** a run with no human turn at all (a
  subagent or a scripted job).
- **File churn**: the files edited more than once in the window, most-edited
  first, each with its edit count and how many sessions returned to it. Rows are
  grouped per project across worktrees, so the same repository file edited from
  several checkouts reads as one row, tagged with its project.

Every grade and outcome bar links into the Sessions feed filtered to that
bucket, so the count you see is the list you land on.

A session is graded only after it settles: half an hour idle, so no verdict is
taken on a run that may still be moving. A session that just finished reads as
unscored (and its outcome as unknown) until then.

## Sessions

**Sessions** is every session across every project in one feed, so you can find a
run without first picking its project. A slim toolbar narrows the feed by
**agent**, **project**, **user**, and **machine**, and sorts it by recency, token
volume, message count, or cost; active filters show as removable chips. A search
box narrows to sessions whose transcript contains the query, composing with every
other filter; a matching row shows a snippet with the hit highlighted. Every other
row carries its **first-prompt title**, the opening line of what the session was
asked to do. The feed loads a page at a time with a **Show more** control, and
sessions that parsed to no messages are hidden behind a toggle. This is the place
to answer "where is that run I did last Tuesday."

A toolbar above the feed adds **outcome** and **grade** filters (how a session
ended, and its letter grade), the same buckets the [Insights](#insights)
distributions and the project view's quality panels break down; clicking a bar
on either lands you on this feed already filtered to it.

## Projects

The **Projects** index is one full-width table of git-remote projects. Each row
carries the project's session count, a single token total (hover it for the
input/output/cache-read/cache-write breakdown), its cost, a 30-day cost
**sparkline**, and a relative "updated" time. Fleet-wide usage lives on the
Overview, and standalone or orphaned local folders reach you through the Sessions
project filter, so neither crowds this table.

Click a project for the **project view**: that project's sessions across all users
and machines, with agent, user, and machine filters and the same analytics panel
as the overview, scoped to the project and its trailing window. A **Quality band**
below it breaks the same scope down by grade, outcome, and archetype, with tool
reliability and a churn list of files edited more than once; the grade and outcome
bars link into this project's filtered sessions.

## The session view

Clicking a session opens the deep read. A sticky stats header keeps the session's
gauges in view as you scroll: tokens in, out, and cached; cost; duration; message
counts; and a **Quality tile** carrying the session's grade and outcome, which
reveals a score-arithmetic breakdown (each penalty and its points) on hover. Below
it is the transcript itself:

- **Messages, thinking, and tool calls**, in order, with a timeline rail that maps
  the turns and flags any tool that errored. Each turn carries a reply-latency
  stamp, a per-message context size (for example "ctx 82k"), and a cost stamp whose
  tooltip breaks the cost down by token class; a divider marks a context shed (for
  example "context shed: 356k -> 66k"), the sharp drops that read as a compaction
  or a clear. A user message carries a prompt-hygiene badge (terse, no code
  pointer, repeat) where it applies.
- **Tool bodies as chips.** A tool call's input and result show as
  size-and-type chips (for example "36 KB json"). Clicking one opens the body in
  the inspector modal, fetched from content-addressed storage on demand, so a
  large body gets real room without pushing the transcript around. An editing
  tool's input opens as a rendered **diff** rather than raw JSON. A chip's file
  path shows worktree-relative rather than absolute; a tool with no file path,
  such as a shell command or a search pattern, instead carries a one-line summary
  of its input on the chip, with the full text on hover.
- **Subagents** spawned by the session are listed under it, so a run that launched
  helpers reads as a tree rather than scattered rows.
- **Live updates.** A session still being written updates in place over
  server-sent events as new bytes are parsed, so you can watch a run unfold.

From the session page its owner (or an admin) can publish, unpublish, or delete
it; [Accounts and sharing](./accounts-and-sharing.md#sharing-a-session) covers
what each does.

## Account

The **Account** page is your control surface:

- **API tokens**: create and revoke tokens in the `ingest`, `read`, or `full`
  scope. The plaintext token is shown once, at creation.
- **Connected apps**: the coding agents you have connected over
  [MCP](./agent-access.md), each with a one-click disconnect that revokes its
  tokens at once.
- **Publicity**: publish or unpublish your own usage overview at `/u/<username>`.
- **Invites** (admins only): mint an invite token for a new teammate, and see
  every invite ever issued with its status (unused, redeemed by whom, or
  expired) and a revoke control for the ones still open.
- **Reparse** (admins only): force a rebuild of the parsed projection, with a live
  progress bar. The Account page stays available during a reparse, since it is not
  parsed data and it hosts this control.

---

Next: [Accounts and sharing](./accounts-and-sharing.md) -> registration, the three
token scopes, and publishing.
