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

**Insights** is the fleet's quality surface over the same trailing window:
concurrency and velocity (how many sessions run at once, how fast the agent
replies), tool reliability and mix, prompt hygiene (terse, repeated, or
no-code-context prompts), context load and reset frequency, and how sessions
distribute across quality grades, outcomes, and archetypes. The grade and outcome
bars, and the busiest-user figure, link through to the Sessions feed pre-filtered
to that slice and window, so you can go from "why are so many sessions graded D"
to the transcripts in one click.

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

## Projects

The **Projects** index is one full-width table of git-remote projects. Each row
carries the project's session count, a single token total (hover it for the
input/output/cache-read/cache-write breakdown), its cost, a 30-day cost
**sparkline**, and a relative "updated" time. Fleet-wide usage lives on the
Overview, and standalone or orphaned local folders reach you through the Sessions
project filter, so neither crowds this table.

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
  size-and-type chips (for example "36 KB json"). Clicking one opens the body in
  the inspector modal, fetched from content-addressed storage on demand, so a
  large body gets real room without pushing the transcript around. An editing
  tool's input opens as a rendered **diff** rather than raw JSON.
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
