# UI revamp plan: usable, approachable, performant

Status: proposed. Written 2026-07-04 from a full walkthrough of the live app at
akari.jessica.black plus a code audit of `internal/server/web/` and
`internal/server/httpapi/`. This plan is the working brief for the implementing
agents; each workstream is scoped so it can be picked up independently.

## The lens

Design every surface for one person: a team lead auditing the efficacy of their
coding agents. Their questions, in the order they ask them:

1. What did my agents do this week, and did it succeed?
2. What did it cost, and where is money being wasted?
3. Which sessions went badly and deserve a closer look?
4. What patterns should change (models, prompts, tooling, delegation)?

Today the app answers question 2 well, answers 1 and 4 only if you already know
how to read the instruments, and barely answers 3 at all. The raw material for
all four exists in the store (`session_signals` carries outcome, grade, tool
health, prompt hygiene, context health, observed thinking per session). The
revamp is mostly about surfacing what is already computed, cutting noise, and
making the two slow paths fast.

## What exists today

Pages: Overview (cost/token/cache/session tiles, activity heatmap, by-model and
by-agent bars), Insights (six instrument groups: Fleet mix, Session gallery,
Velocity, Tools, Health, Economics, Subagents), Sessions (search, four
single-select filter dropdowns, sort, day-grouped feed), Session detail (stat
band, subagents table, outline rail, full transcript, tool-body modal, SSE live
updates), Projects (table with sparklines), Project detail (stats, heatmap,
quality band, tool mix, file churn, session list), Account, Guide, and public
mirrors (`/u/{username}`, `/s/{public_id}`, `/p/{id}`).

Stack facts that constrain the work (from the code audit):

- Server-rendered templ + htmx, no Node build step. Charts are two hand-rolled
  SVG engines: `static/charts.js` (254 lines, Overview) and
  `static/js/insights.js` (2,260 lines, Insights) reading one inline JSON blob
  per page (`web/insights_data.go`).
- All routes register in `httpapi/server.go` `Routes()`; page handlers live in
  `httpapi/web.go`.
- `/insights` calls `Store.Insights` (`store/analytics_insights.go:56`), which
  runs roughly 18 aggregate queries sequentially in one repeatable-read
  transaction, no caching, no rollups. Measured: 4.6s server time for the Year
  window on ~3,650 sessions.
- The sessions feed (`Store.ListAllSessions`) is a single indexed query and
  fast (64ms measured). Its "Show more" doubles `limit` (100 to 500 cap) and
  re-renders the whole list.
- Session detail server time is fine (73ms, 333KB for a 106-message session)
  but the transcript is fully server-rendered with no pagination, and the SSE
  hook re-fetches and re-renders the entire body on every parse event
  (`app.js` around line 109, `handleSessionBody`). Scrolling a 100-message
  session froze the Chrome renderer twice during the walkthrough.
- `store.SessionFilter` has no field for `relationship_type` or
  `parent_session_id`, so subagent sessions appear in every list identically
  to the sessions that spawned them.
- Design system to preserve: "Machinist's Bench" (Geist/Geist Mono, hairline
  borders, tabular numerics, tags not pills). `docs/ui-proposal.md` is the
  prior IA pass; its sidebar and Overview/Insights split shipped, its saved
  views, live strip, drawer inspector, and command palette did not.

## Measured and observed problems

Performance:

- P1. `/insights` takes 4.6s server-side. Cause: sequential aggregate queries
  over the full window on every request, several of which scan the same
  joins (`sessions` x `session_signals`) independently.
- P2. Long transcripts freeze the renderer. Cause: unbounded server-rendered
  DOM plus full-body re-render on every SSE wake signal. A live session
  re-renders everything each time the parser settles new bytes.
- P3. "Show more" on the feed re-fetches rows 1..N to show N+100, and tops out
  at 500 with a "narrow your filter" message.
- P4. No `Cache-Control`/ETag on any HTML route; back-navigation re-runs the
  full pipeline.

Approachability:

- A1. The sessions feed is unreadable at fleet scale. On the day of the
  walkthrough, roughly 60 of 73 "today" sessions were machinery: Bastion
  review runs, Agent-tool fan-outs, spec-extraction batches. The lead's own
  eight working sessions are buried in them.
- A2. Session rows lead with the raw first prompt, which for spawned sessions
  is a wall of system-prompt boilerplate ("You are reviewing a changeset
  computed against the base branch...", "# AGENTS.md instructions...").
- A3. The project filter dropdown mixes real repositories with dozens of junk
  auto-created entries (slugified first prompts like `what-does-this-say`,
  model names like `claude-opus-4.7-high` used as folder names).
- A4. Grades and outcomes are computed for every settled session but the feed
  shows none of it; grade/outcome exist only as hidden query params reachable
  from Insights drill-downs.
- A5. Insights labels assume the reader knows the schema: captions cite
  `usage_events.model` and `parent_session_id`, tiles say "3.7% no pointer"
  and "104.6k" with no unit or explanation visible without hovering.
- A6. Density: 10pt mono numerics everywhere, twelve-row model breakdowns on
  first paint, forty-row subagent tables above the transcript.

Correctness and polish found during the walkthrough:

- C1. The Subagents fan-out chart on Insights renders an empty plot with a
  literal `NaN` axis label.
- C2. 404s return an unstyled `404 page not found` (no layout, no way back).
  Project URLs are numeric (`/projects/6`), so guessed/stale links 404.
- C3. The session-detail stat band shows quality as a bare `-` for live
  sessions with no hint that grading happens at settle.

## Target information architecture

Keep the five-item sidebar. Change what each surface leads with.

- Overview: becomes the audit dashboard. Answers "what happened, did it work,
  what did it cost, what needs my attention" for a selected window. Detail in
  workstream C.
- Sessions: becomes a feed of work, not of processes. Root sessions only by
  default, subagents rolled up under their parent. Detail in workstream B.
- Insights: keeps the deep instruments (they are good, and the fleet-level
  trend questions need them) but gains a plain-language summary strip and
  loses the jargon captions. Stays the "why", while Overview becomes the
  "what".
- Session detail: progressive disclosure. The audit view first (what was asked,
  what happened, what it cost, the verdict), the full transcript on demand.
  Detail in workstream D.
- Projects: unchanged structurally, but split repositories from local folders
  and inherit the feed fixes.

## Workstream P: performance

### P-1 Insights under 300ms warm, under 1.5s cold

Three changes, in order of leverage:

1. Parallelize `Store.Insights`. The panel queries are independent reads.
   Replace the sequential chain in `analytics_insights.go` with an errgroup
   fanning out over pooled connections (drop the single shared tx; per-query
   snapshot consistency is acceptable for a dashboard, or take a snapshot with
   `pg_export_snapshot` if drift between panels ever matters). Expected: wall
   time drops from sum to max of query times.
2. Consolidate the redundant scans. The grade trend, outcome trend, hygiene
   trend, and context trend each join `sessions` to `session_signals` over the
   same window with the same bucket expression. Collapse them into one
   bucketed CTE returning all four series. Same for the tool trend pair.
3. Cache the assembled `Insights` result in-memory, keyed by
   `(range, parse.Epoch, latest ingest cursor)`, invalidated by the same
   parse-worker hook that feeds the SSE hub. The corpus only changes when the
   rebuild worker writes, so this cache is exact, not a TTL guess. Bound it to
   the handful of named ranges.

Measure before and after with the same probe used for this plan
(`fetch('/insights')` wall time; add a server-side `Server-Timing` header while
at it so future regressions are visible in devtools).

### P-2 Transcript rendering that cannot freeze the tab

1. Cap the initial render at the last 50 turns (a turn: one user message plus
   the assistant run that follows). Render a "Show earlier" bar at the top
   that htmx-fetches the previous 50 into the same list, keyed by message
   sequence, reusing the existing fragment pattern.
2. Make SSE updates incremental. The wake signal should carry (or the client
   should request) only messages after the last-seen sequence:
   `GET /sessions/{id}/body?after={seq}` returns just the new turns, appended
   with `hx-swap="beforeend"`, plus an out-of-band swap for the stat band.
   Today's full-body re-render is both the freeze and a scroll-position reset
   on live sessions.
3. Collapse the subagents table by default when it exceeds 8 rows (summary
   line: "34 subagents, $6.12, 2 failed", expandable). It currently pushes the
   transcript below the fold on exactly the sessions a lead most wants to
   audit.
4. The outline rail renders one entry per turn regardless; it stays, but its
   anchors must work with the windowed transcript (fetch-then-scroll for
   turns not yet in the DOM).

### P-3 Feed pagination

Replace limit-doubling with keyset pagination: "Show more" passes the last
row's `(last_active_at, id)` cursor and appends the next 100 rows
(`hx-swap="beforeend"` on the list body). Remove the 500 cap; the cursor makes
depth cheap. The existing `(last_active_at DESC, id DESC)` indexes are exactly
right for this.

### P-4 HTTP hygiene

ETag or short `Cache-Control: private, max-age=30` on `/overview`, `/projects`,
`/insights` (the cache from P-1 makes ETags nearly free: hash the cache key).
Not worth doing for `/sessions` (already fast, changes constantly).

## Workstream B: the sessions feed reads as work

### B-1 Root sessions by default

Add `RelationshipType` filtering to `store.SessionFilter` and
`ListAllSessions`. Default the feed to `relationship_type = ''` (roots).
Toggle in the toolbar: "Include subagents" (off by default), plus a
`continuations` treatment: a continuation chains to its predecessor, show only
the latest with a "resumed x2" marker. The facet counts
(`session_facets`) must reflect the same default or the dropdown numbers will
contradict the list.

### B-2 Roll the tree up into the parent row

A root session's row shows tree totals: cost, tokens, and subagent count
including descendants ("$4.12 · 62 subagents"). Two implementation options;
pick one and commit:

- Compute at read time with a recursive CTE bounded to the visible page of
  parents (depth is 2 today, cheap), or
- Maintain `tree_cost_usd`/`tree_token`/`subagent_count` as rebuild-derived
  columns. This is cleaner for sorting ("most expensive work item") but
  touches the rebuild path, which means a `parse.Epoch` bump and a corpus
  rebuild (see Constraints).

Read-time CTE first; promote to derived columns only if the page-of-parents
query shows up in timings.

### B-3 Rows lead with a title, not a prompt dump

Derive a display title server-side at render time (no schema change):

- Strip harness preambles before taking the first line: `<command-message>`,
  `<local-command-caveat>`, `# AGENTS.md instructions`, Bastion's "You are
  reviewing a changeset..." and similar known prefixes live in one strippable
  list.
- Spawned-session rows (visible when the toggle is on, and in the parent's
  subagent table) get their Agent-tool description when the parser captured
  one, falling back to the stripped prompt.
- Add the grade letter and outcome to the row (A4): a small grade chip
  colored by band, a muted outcome word for anything not completed. Rows for
  unmeasured sessions show nothing rather than `-`.

### B-4 Projects that are not projects

Split the project facet and the Projects page into "Repositories" (git-keyed)
and "Local folders" (path-keyed), with local folders collapsed behind one
entry in the filter dropdown. This is presentation only; the store already
distinguishes them (the Projects page footnote says local folders live under
Sessions, but the sessions filter mixes both).

## Workstream C: Overview becomes the audit dashboard

Top of page, for the selected window (keep the range selector):

1. Verdict strip: four tiles that answer the lead's first two questions.
   "N work items" (root sessions), "82% completed" with the grade GPA beside
   it, total spend, and wasted spend (cost of abandoned/errored sessions, the
   `Cost of quality` number Insights already computes). Each tile links to
   the filtered feed or the Insights instrument behind it.
2. Needs attention: a short list (max 10) of sessions worth a human look,
   ranked by a simple score: errored or abandoned, grade D/F, cost above the
   window's p90, longest failure streak. Each row: title, grade, outcome,
   cost, one-line reason ("errored after $12.40", "F: 14 tool failures").
   This is the single highest-value addition in the plan; it is question 3
   and no current surface answers it.
3. Keep the heatmap and the by-model/by-agent bars below, collapsed to the
   top 5 rows with "show all".

The existing tiles (cost, tokens, cache, session count) fold into the verdict
strip; tokens and cache move down beside the model bars. All data needed here
already exists in `AnalyticsSnapshot` and the Insights queries; the work is
one new store query for the attention list and a rebuilt `overview.templ`.

## Workstream D: session detail for auditors

1. Lead with the audit header: title, verdict (grade + outcome + score
   arithmetic already in the quality tooltip, promoted to a visible line),
   cost, duration, model(s), then the stat band. For live sessions, replace
   the bare `-` with "grading after session settles" (C3).
2. Progressive transcript per P-2. Tool chips stay, but tool bodies keep the
   modal for now (the drawer from ui-proposal.md is a nice-to-have, not this
   pass).
3. Subagent table collapsed per P-2, with tree cost rollup per B-2.
4. A "flow" ribbon above the transcript: the existing outline data rendered
   as a horizontal strip of turn ticks colored by activity (edit, run,
   failure), so a reviewer sees the shape of the session (long failure
   streaks, churn loops) before reading a word. This reuses the outline
   model; it is presentation only.

## Workstream E: approachability pass (copy, labels, fit and finish)

1. Every instrument on Insights gets a one-line plain-language caption first
   ("Which models handled the work over time"), with the schema-flavored
   caption demoted to a hover/details line. Kill raw column names in body
   copy (A5).
2. Units on every number tile ("104.6k" becomes "104.6k tokens peak context").
3. A summary strip at the top of Insights: three sentences generated from the
   data ("Quality held at GPA 3.69 while spend rose 40% week over week; tool
   failure rate 1.3%; 12% of spend went to abandoned sessions."). The store
   already returns everything needed.
4. Fix C1 (NaN fan-out chart): the depth-distribution series is empty when no
   session in the window nests deeper than 1; guard the domain computation
   and render the "not enough data" empty state the other instruments have.
5. Styled 404/error pages using `errors.templ`'s layout for both authed and
   public routes (C2), with links back to Overview/Sessions.
6. Sweep prose for em/en dashes and replace with plain punctuation, per repo
   style.

## Constraints the implementers must hold

- No Node build step, no framework. templ + htmx + vanilla JS stays. The two
  SVG chart engines stay unless a workstream above explicitly touches them.
- Machinist's Bench design tokens stay: Geist/Geist Mono, hairline borders,
  tabular numerics, tags not pills, dark surface.
- Never write projection rows outside the rebuild path. If any workstream
  stores a new rebuild-derived column (B-2's option 2), it bumps `parse.Epoch`
  in the same commit and expects the golden-fixtures test to fail by name
  until it does. Read-time computation avoids this entirely; prefer it first.
- `make generate` after touching any `*.templ`; `eph run go test ./...` is the
  test gate (the store/web integration tests silently skip without a
  database).
- Every current capability survives: publish/unpublish, delete, density
  toggle, public pages, SSE live view, MCP surface, OG cards.

## Sequencing and assignment

Order matters only where noted; A/B tracks can run in parallel worktrees.

| Phase | Work | Assignee guidance |
|-------|------|-------------------|
| 1 | P-1 (Insights parallelize + consolidate + cache), P-4 | gpt-5.5: pure backend, measurable target, clear spec |
| 1 | C1, C2, C3 bug fixes | gpt-5.5: small, self-contained |
| 2 | B-1, B-3 store + feed changes; P-3 keyset pagination | gpt-5.5 for store/filter plumbing; opus-4.8 for row layout and title heuristics |
| 2 | P-2 transcript windowing + incremental SSE | gpt-5.5, spec above is tight; verify against a live session |
| 3 | C (Overview audit dashboard), B-2 rollups | opus-4.8: this is the taste-heavy surface; gpt-5.5 supplies the attention-list query |
| 3 | D (session detail header, flow ribbon) | opus-4.8 |
| 4 | E (copy pass, Insights summary strip, B-4 project split) | opus-4.8 for copy; either for mechanics |

Review gate: every phase lands as its own PR, reviewed by fable-5 (or
opus-4.8), with Bastion green. Phase 1 needs a before/after timing table in
the PR description.

## Acceptance criteria

- `/insights` p50 under 300ms warm and 1.5s cold on the production corpus,
  verified by `Server-Timing` and the fetch probe.
- A 300-message session scrolls end to end with no long task over 200ms
  (Chrome performance trace); live updates append without scroll reset.
- Default `/sessions` shows only root sessions; the walkthrough day that
  showed 73 rows shows roughly 13, each with a readable title, grade chip
  where measured, and rolled-up cost.
- Overview answers, above the fold and without hovering: how many work items
  ran this week, what fraction completed and at what GPA, total and wasted
  spend, and which specific sessions need review.
- No `NaN` renders anywhere on Insights for any range; 404s are styled.
- `go test ./...` green under eph; golden fixtures updated only if an epoch
  bump was actually taken.

## Explicitly out of scope this pass

Saved views and the command palette (ui-proposal.md phase 2 items), the
tool-body drawer, "needs input" live state (requires client-side signals the
ingestion model does not have), mobile layout, and any chart-engine swap.
