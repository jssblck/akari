# Insights rollups: a materialization plan

Status: implemented (migration 0048; derivations in
`internal/server/store/rollups.go`; epoch 15). The plan below is kept as the
design rationale. The implementation deviates from it in three places:

- **The usage cluster conversion is scoped to the Insights consumers.**
  `fleetMixFrom`, `economicsFrom`, `cacheSavingsTrend`, and the subagent cost
  share read `session_usage_daily`; `Analytics`, `cacheStats`, and
  `ProjectSparklines` stay on the `usage_events` ledger. Those surfaces window
  on raw instants (`occurred_at >= now - Nd`), and a day-grain rollup cannot
  reproduce a sub-day bound; keeping them on the ledger preserves their exact
  semantics and their role as the reconciliation oracle
  (docs/data-aggregation.md, "Whole-day windows on the daily rollup", covers
  the Insights side's deliberate whole-day lower bound).
- **`session_turns` stores `response_secs DOUBLE PRECISION`, measured turns
  only,** rather than the plan's nullable `response_ms INT`: the float keeps
  `extract(epoch ...)` verbatim so percentiles interpolate unchanged, and an
  unmeasured turn contributed nothing to any consumer, so storing it bought
  nothing.
- **The announce path re-derives `session_file_churn`.** An announce that
  changes a session's cwd rewrites `tool_calls.file_rel_path` in place (the one
  projection write outside the rebuild), so the churn rollup re-derives in that
  same transaction (`ingest.go`), which the plan had not called out.

## The problem

A cold load of `/insights` runs roughly twenty aggregate queries over the whole
trailing window (`Store.Insights`, `internal/server/store/analytics_insights.go`),
which takes several seconds. Two mitigations already exist and both are
palliative:

- The panels fan out over up to six pooled connections sharing one exported
  MVCC snapshot, so wall time is the slowest panel rather than the sum
  (`analytics_insights.go:99`). The slowest panel still scans `messages`.
- The fleet page memoizes the computed `store.Insights` struct per range for
  60 seconds behind a singleflight
  (`internal/server/httpapi/insights_cache.go`). Every cold key still pays the
  full compute, once a minute per range in the worst case.

The project page is worse off on both counts: `handleProjectPage`
(`internal/server/httpapi/web.go:508`) calls the same `Store.Insights` with
`ProjectID` set and no cache at all, and because `Bucket` is always set, the
store computes the full trend pipeline (gallery, velocity, economics, rhythm,
subagents) even though the project template renders only the quality series and
the tools instrument. Every project page load pays for five instruments nobody
sees.

The only timing instrumentation is the `Server-Timing: insights;dur=...` header
on `/insights` (`web.go:198`). There is no benchmark or recorded baseline.

## Where the time goes

The full query inventory (48 query sites) sorts into four cost classes by the
table they scan:

1. **`messages`, scanned with window functions.** Velocity latency percentiles
   label turns with `count(*) OVER (PARTITION BY session_id ORDER BY ordinal)`
   and take `percentile_cont` over the gaps; active time runs `lag()` per
   session; the rhythm punchcard bins every message by hour of week. Eight query
   sites, no supporting index on timestamps, and `messages` is the largest
   table. This is the dominant cost.
2. **`tool_calls`, scanned with a dedup window function.** Every tools, failure,
   and churn query re-runs `row_number() OVER (PARTITION BY <dedup key>)` to
   collapse replayed calls. Eight query sites over the second-largest table,
   each repeating the same dedup.
3. **`usage_events`, grouped by day and model.** Well indexed and already shaped
   like a rollup read, but it is one row per model turn and feeds the usage
   panel, fleet mix, economics, cache savings, and sparklines on every load.
4. **`sessions` joined to `session_signals`.** One row per session; grades,
   outcomes, archetypes, hygiene, context, gallery, leaderboard, concurrency,
   subagents. Already cheap: `session_signals` is itself a per-session rollup
   graded off the hot path.

Class 4 is fine. Classes 1 through 3 are the materialization targets.

Which timestamp buckets each trend matters for the design. There are exactly
three: `s.started_at` (signals, archetypes, tools, failures, churn, wall span,
subagent counts), `ue.occurred_at` (fleet mix, economics, cache savings,
subagent cost share), and per-message instants (velocity latency buckets on the
turn's prompt time, active time and tool volume on `m.timestamp`, rhythm on the
hour of week of `m.timestamp`). Anything bucketed on `s.started_at` can live at
plain session grain and pick up its bucket by joining `sessions`; only the
usage- and message-bucketed series need a time dimension in the rollup itself.

## Decision: rollup tables maintained in the rebuild, not materialized views

The question posed was materialized tables akari maintains versus Postgres
materialized views on a refresh cadence. The plan is tables, written inside the
existing per-session rebuild transaction. The reasons to reject `CREATE
MATERIALIZED VIEW` with a refresh loop:

- **A refresh recomputes the whole aggregate.** `REFRESH MATERIALIZED VIEW`
  re-runs the full defining query. The scan cost does not shrink; it moves off
  the request path onto a timer and is paid whether or not anyone loads the
  page. Incremental view maintenance is not in stock Postgres.
- **The filter space cannot be enumerated.** Insights filters by project, user,
  agent, machine, and five ranges at two bucket widths. A materialized view
  materializes one query. Serving the project page and the filter chips would
  need either one view per filter combination (unbounded) or views wide enough
  in dimensions that they are just rollup tables with worse maintenance.
- **Multi-instance refresh needs coordination akari does not have.** The server
  is explicitly designed so N instances need no coordination
  (docs/DESIGN.md, "Multiple server instances need no coordination"). A refresh
  cadence either runs redundantly on every instance (paying the full recompute
  N times) or requires the leader election the design deliberately avoids.
  Non-concurrent refresh also takes an ACCESS EXCLUSIVE lock that blocks reads;
  `CONCURRENTLY` trades that for a unique index requirement and a full diff.
- **Pricing lives in Go.** Cache savings are priced per model and day by
  `pricing.CacheSavings`, compiled into the binary
  (`analytics_cache.go`). A pure SQL view cannot produce the figure; the read
  path must end in Go regardless, so the database-side artifact should be the
  grouped token sums, which is exactly a rollup table.
- **The codebase already has the right write hook.** Parsing is
  rebuild-on-dirty: `rebuildTx` (`internal/server/store/projection.go:262`)
  deletes and rewrites a session's entire projection in one transaction, under
  a row lock, and sets `sessions.total_*` absolutely from the folded rows so
  the rollup equals the ledger by construction
  (docs/data-aggregation.md). Rollup tables at session grain slot into that
  transaction as two more statements and inherit exactness, atomicity, and
  multi-instance safety for free. A cadence would add a second freshness
  mechanism beside one that already exists and is already correct.

The trigger pattern from `session_facets` (migration 0005) was also considered
and rejected for these tables: a rebuild deletes and reinserts every projection
row for the session, so delta-computing triggers would fire per row across the
rewrite and have to net out to the same answer. Deriving the session's rollup
rows once, after the rewrite, in the same transaction, is simpler and exact.

## Design

### The grain rule

Every rollup keys on `session_id` and carries no dimension columns that
`sessions` or `session_signals` already carry. Project, user, agent, machine,
grade, and outcome come from the join at read time. Two consequences:

- No dimension explosion, and no coupling to signals grading: a session getting
  graded (or re-graded) never touches a rollup row, because outcome is joined,
  not stored. The economics split by outcome stays correct the moment a grade
  lands, with no rollup write.
- Rollups are written exclusively by `rebuildTx` and deleted by session
  cascade, the same lifecycle as every other projection table. Nothing else
  writes them.

### One derivation, used twice

Each rollup is defined by a single SQL statement over the projection tables
(`INSERT INTO <rollup> SELECT ... FROM messages/tool_calls/usage_events WHERE
session_id = $1`), held as a named constant. `rebuildTx` runs it per session
after the projection rewrite; the backfill migration runs the same statement
without the session predicate to populate the corpus. The rollup can never
disagree with a fresh derivation over the projection, because it is the same
SQL, and the reconciliation tests re-run the derivation and compare (the
`TestSessionRollupMatchesLedger` pattern).

This also reuses the queries we already trust: the turn-labeling expression and
the dedup partition move from the read path into the derivation, verbatim.

### The tables

Migration `0048_insights_rollups.sql` creates five tables.

```sql
-- Usage grouped the way every consumer already groups it. day is the UTC day
-- of occurred_at; NULL preserves the documented undated-usage gap
-- (docs/data-aggregation.md): rollup-base consumers count it, dated analytics
-- exclude it.
CREATE TABLE session_usage_daily (
  session_id         BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  day                DATE,
  model              TEXT NOT NULL,
  input_tokens       BIGINT NOT NULL,
  output_tokens      BIGINT NOT NULL,
  cache_read_tokens  BIGINT NOT NULL,
  cache_write_tokens BIGINT NOT NULL,
  cost_usd           DOUBLE PRECISION NOT NULL,
  cost_incomplete    BOOLEAN NOT NULL
);
CREATE INDEX idx_session_usage_daily_session ON session_usage_daily (session_id);
CREATE INDEX idx_session_usage_daily_day ON session_usage_daily (day) WHERE day IS NOT NULL;

-- Tool calls per (tool, category), deduplicated once at write time with the
-- same partition the read queries use today. Buckets come from s.started_at,
-- so no time dimension.
CREATE TABLE session_tool_rollup (
  session_id BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  tool_name  TEXT NOT NULL,
  category   TEXT NOT NULL,
  calls      INT NOT NULL,
  failures   INT NOT NULL,
  PRIMARY KEY (session_id, tool_name, category)
);

-- Edit-category calls per file, deduplicated. Project and folder come from the
-- sessions/projects join.
CREATE TABLE session_file_churn (
  session_id    BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  file_rel_path TEXT NOT NULL,
  edits         INT NOT NULL,
  PRIMARY KEY (session_id, file_rel_path)
);

-- One row per prompt-to-reply cycle: the pre-labeled turn the velocity
-- percentiles need raw values from. percentile_cont runs over this table
-- directly; the count() OVER turn labeling and DISTINCT ON first-reply pick
-- happen once, at write time.
CREATE TABLE session_turns (
  session_id  BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  turn        INT NOT NULL,
  prompt_at   TIMESTAMPTZ NOT NULL,
  response_ms INT,           -- NULL when the turn never got a timestamped reply
  PRIMARY KEY (session_id, turn)
);

-- Message and tool-call volume plus gap-filtered active seconds per UTC
-- (day, hour). Serves the active-time and tool-volume trends (day sums roll up
-- exactly to the Monday-anchored week buckets) and the hour-of-week rhythm
-- grid (dow derives from day).
CREATE TABLE session_activity_hourly (
  session_id     BIGINT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
  day            DATE NOT NULL,
  hour           SMALLINT NOT NULL,
  messages       INT NOT NULL,
  tool_calls     INT NOT NULL,
  active_seconds INT NOT NULL,
  PRIMARY KEY (session_id, day, hour)
);
```

The same migration adds `CREATE INDEX idx_sessions_project_started ON sessions
(project_id, started_at)`: today every project-scoped Insights query leans on
the bare `started_at` index plus a filter, and the project page is the surface
this plan most needs to fix.

Sizing: every table is O(sessions) times a small factor (models times active
days, distinct tools, churned files, prompts, active hours), where `messages`
and `tool_calls` are O(events). On the corpora that make the page slow, that is
one to two orders of magnitude fewer rows, read without window functions.

### rebuildTx changes

Two additions to `rebuildTx` (`projection.go:262`):

1. The five rollup tables join the DELETE list at the top.
2. After `writeMessages`/`writeToolCalls`/`writeUsage`, run the five derivation
   statements for the session.

Per-rebuild cost is a scan of the session's own just-written rows, the same
order as the rewrite itself. Live sessions rebuild on every chunk append, so
rollups track live sessions with no extra freshness machinery.

### What stays unmaterialized

Everything in cost class 4 keeps reading `sessions` and `session_signals`
directly: grade and outcome distributions and trends, archetypes, hygiene,
context health, the gallery, the leaderboard, wall span, subagent counts and
depth, and the concurrency sweep line (which needs exact start and end instants
and never touches the big tables). These are one indexed row per session
already; materializing them further would add tables without removing a scan
that matters. If a future measurement says otherwise, the same grain rule
applies.

`SessionCacheStats` also keeps scanning `usage_events`: it is per-session,
off the hot path, and serves as the independent oracle the reconciliation
tests price the rollups against.

## Consistency and rollout

### Backfill plus an epoch bump, and why both

The migration backfills all five tables from the existing projection rows, so
the corpus is complete the moment the new binary starts. No epoch drain gap: the
charts never dip while a reparse catches up.

`parse.Epoch` gets bumped in the same commit anyway, for a reason the backfill
does not cover: a rolling deploy. After the migration runs, an old binary's
`rebuildTx` can still rebuild a session; it rewrites the projection without
touching the rollups, leaving that session's rollup rows describing the
pre-rebuild projection, and nothing would ever mark it due again. The bump makes
every session due at the new epoch, and the old binary stamps the old epoch, so
whatever it rebuilt stays due until a new binary re-derives it. The background
reparse is redundant work for sessions the backfill already covered, and it is
exactly the mechanism the pipeline is designed around (docs/DESIGN.md, "the
monotonic due predicate keeps the binaries out of each other's way").

Going forward, the rule joins the signals.md list explicitly: changing a rollup
derivation is a rebuild-derived-output change and takes an epoch bump. The
reducer's output does not change in this plan, so the golden projection
fixtures do not change. The goldens also cannot police the derivations: the
golden test runs the reducer in memory with no database, and the derivations
are SQL over the projection. The reconciliation tests are the guard for
derivation correctness; the epoch rule for derivation changes is enforced the
way the pricing-table rule is, by the documented list and review.

### Reconciliation tests

Per docs/data-aggregation.md, each new base gets pinned:

- `TestSessionUsageDailyMatchesLedger`: per session, per day, per model, the
  rollup equals the `usage_events` sums, and the undated gap stays exactly the
  NULL-day rows (extend `TestUndatedUsageIsTheOnlyRollupAnalyticsGap`).
- `TestToolRollupMatchesDedupedCalls` and
  `TestFileChurnMatchesDedupedEdits`: rollups equal the dedup-windowed
  derivation re-run over `tool_calls`.
- `TestTurnsMatchMessages` and `TestActivityMatchesMessages`: turn labeling,
  response latencies, counts, and gap-filtered active seconds equal the
  old read-path expressions re-run over `messages`.
- Panel-level oracles: for each converted panel, the integration test runs the
  old query (kept as an unexported test helper) and the new rollup-based query
  against the same seeded corpus and compares results, then the old query is
  deleted with the conversion. Ingest-then-rebuild and epoch-rebuild paths both
  covered, as `TestSessionRollupMatchesLedger` does today.

docs/data-aggregation.md gains a row per rollup in its bases table, and the
DESIGN.md schema section gains the five tables (and should pick up
`session_facets`, which it currently omits).

## Read-path conversion

Panel by panel, each landing with its oracle test:

1. **Usage cluster** (biggest shared win, also speeds the overview):
   `Analytics` series/by-model/by-agent/by-user, `cacheStats`,
   `ProjectSparklines`, `fleetMixFrom`, `economicsFrom` (outcome via the
   signals join), `cacheSavingsTrend`, and the subagent cost share move to
   `session_usage_daily`.
2. **Tools cluster**: `toolStatsFrom`, `toolTrendsFrom`, `toolFailureTrend`
   move to `session_tool_rollup`; the dedup window function disappears from the
   read path.
3. **Churn cluster**: `fileChurnFrom`, `churnTrendFrom` move to
   `session_file_churn`.
4. **Velocity and rhythm cluster**: `velocityStatsFrom` and `velocityTrendsFrom`
   read `session_turns` (percentiles over pre-labeled turns) and
   `session_activity_hourly` (active seconds, message and tool volume);
   `rhythmFrom` reads `session_activity_hourly`. `messages` leaves the Insights
   read path entirely.

The snapshot fan-out machinery needs no change: the converted panels are still
plain queries inside the shared snapshot. Once measured, the fan-out itself may
be unnecessary (twenty cheap reads may beat six borrowed connections), but that
is a cleanup to earn with numbers, not part of this plan.

## Stop computing what the page does not draw

Independent of materialization, `Store.Insights` gains a panel-set parameter so
callers name the instruments they render. The fleet page asks for everything;
the project page asks for signals trends plus tools plus churn and skips
gallery, velocity, economics, rhythm, and subagents; the public project page
already skips trends by leaving `Bucket` empty. This alone removes more than
half the project page's query count and is worth landing first, before any
schema work, since it is a day of work and makes the baseline honest.

The 60-second fleet cache stays (it still absorbs burst loads for free), but
after conversion the cold path should be tens of milliseconds, and the cache
stops being the thing standing between the user and a multi-second render. The
uncached project page should not need a cache at all.

## Phases

1. **Baseline.** `eph up` with seeded data, record `Server-Timing` for `/insights`
   (each range, cold and warm) and a project page. Add the same header to the
   project page handler while there. Record numbers in the PR.
2. **Panel selection.** The panel-set parameter; project page stops computing
   the five unrendered instruments. Re-measure.
3. **Schema and write path.** Migration 0048 (five tables, backfill, the
   `(project_id, started_at)` index), `rebuildTx` derivations, epoch bump,
   reconciliation tests, docs updates.
4. **Read conversion.** The four clusters above, in order (usage, tools, churn,
   velocity/rhythm), each with its oracle test, deleting each old query as its
   replacement lands.
5. **Verify and clean up.** Re-measure against phase 1 on the same corpus,
   confirm `go test ./...` under `eph run` (integration tests need the
   database), reconcile docs, and decide with numbers whether the panel fan-out
   is still pulling its weight.

## Success criteria

- Cold `/insights` and project page renders drop from seconds to well under
  half a second on the corpus that motivated this (target: the slowest panel no
  longer scans `messages` or `tool_calls`).
- Every rollup is pinned to its ledger by a reconciliation test, and the
  documented rollup-versus-analytics gap remains exactly the undated usage.
- No second freshness mechanism: rollups are exactly as fresh as the projection,
  because they are written in the same transaction.

## Open questions

- **Active-seconds attribution at hour boundaries.** The current query
  attributes a gap to the bucket of the later message. The derivation should
  keep that (attribute the whole gap to the later message's hour) for exact
  equivalence; splitting gaps across hours would be more honest for the
  punchcard but changes numbers. Default: keep current semantics, note it in
  the derivation comment.
- **Backfill duration.** The backfill runs inside the startup migration
  transaction. On a large corpus the `session_turns` and
  `session_activity_hourly` derivations scan all of `messages` once; that is
  the same scan one cold Insights load does today, so minutes at the very
  worst, but it blocks startup for that one deploy. If that is unacceptable for
  some instance, the fallback is to ship the backfill as a due-driven pass in
  the parse worker instead; the epoch bump already makes every session due.
- **Tool reliability grain.** The reliability scatter draws the top 60 tools
  over the window; `session_tool_rollup` serves it exactly. If a future panel
  wants per-tool trends bucketed by call time rather than session start, the
  rollup needs a day column. Nothing rendered today needs it, so it is left
  out.
