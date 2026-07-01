# The web UI

> Reading your history: the overview, the session feed, projects, and the
> transcript view.

The web UI is where a human reads what the agents did. It is a server-rendered,
dense-but-calm instrument: a persistent left sidebar carries the primary sections
(Overview, Projects, Sessions, Account), with the signed-in user and a log-out
control at its foot. Reading the UI needs a full-scope credential, which in
practice is a browser session; signing in gives you that.

## Overview

**Overview** is the landing surface: fleet-wide usage bounded to a trailing
window. Pick the window (7, 30, or 90 days, a year, or all of history) and every
figure on the panel follows it:

- **Cost, combined tokens, and session totals** for the window, as stable tabular
  figures. A partial cost (some session used a model the rate table does not
  price) shows a trailing `+`.
- **A daily-activity heatmap**, one cell per day, so a busy stretch or a quiet one
  is visible at a glance.
- **By-model and by-agent breakdowns** of where the usage went.

You can also scope the overview to specific accounts. It is the wide-angle view;
the deep read is a single session.

## Sessions

**Sessions** is every session across every project in one feed, so you can find a
run without first picking its project. A faceted filter rail down the side lets you
narrow by **agent**, **project**, **user**, and **machine**, each option carrying
a count, and a project column shows where each row belongs. Column headers sort the
feed. This is the place to answer "where is that run I did last Tuesday."

## Projects

The **Projects** index is one full-width table of git-remote projects. Each row
carries the project's session count, a single token total (hover it for the
input/output/cache-read/cache-write breakdown), its cost, a 30-day cost
**sparkline**, and a relative "updated" time. Fleet-wide usage lives on the
Overview, and standalone or orphaned local folders reach you through the Sessions
filter rail, so neither crowds this table.

Click a project for the **project view**: that project's sessions across all users
and machines, with agent, user, and machine filters and the same analytics panel
as the overview, scoped to the project and its trailing window.

## The session view

Clicking a session opens the deep read. A sticky stats header keeps the session's
gauges in view as you scroll: tokens in, out, and cached; cost; duration; and
message counts. Below it is the transcript itself:

- **Messages, thinking, and tool calls**, in order, with a timeline rail that maps
  the turns and flags any tool that errored.
- **Tool bodies as chips.** A tool call's input and result show as
  size-and-type chips (for example "36 KB json") that expand inline when you click
  them, fetched from content-addressed storage on demand. An editing tool's input
  expands as a rendered **diff** rather than raw JSON.
- **A density toggle** switches between a comfortable and a compact reading mode,
  for skimming a long run or settling into a close read.
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
- **Invites** (admins only): mint an invite token for a new teammate.
- **Reparse** (admins only): force a rebuild of the parsed projection, with a live
  progress bar. The Account page stays available during a reparse, since it is not
  parsed data and it hosts this control.

## A note on the reader's experience

akari is built for long reading of large transcripts: figures are tabular and do
not jitter as data streams in, status is carried by more than color, motion
communicates state rather than decorating, and reduced-motion preferences are
honored.

---

Next: [Accounts and sharing](./accounts-and-sharing.md) -> registration, the three
token scopes, and publishing.
