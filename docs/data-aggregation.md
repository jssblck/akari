# Token and cost aggregation: bases and invariants

This is the inventory the audit in issue #41 asked for: every place akari
aggregates token or cost data, which base each reads, and the invariant that keeps
the bases agreeing. It exists because overlapping views that each derive "the same"
number from a different query have nothing forcing them to agree, and #40 was the
bug that shape produced (the overview headline diverging from the rows beneath it
by an order of magnitude).

This is the token/cost instance of a general pattern: any maintained projection of
the same underlying data (a denormalized rollup, a counter, a facet, a cache, a
running hash) can drift from its source unless something forces it equal. The
`projection-consistency` bastion reviewer enforces the general rule across the
codebase; this document is the worked example, the one cluster written out in full.

The rule the codebase follows now:

> When several views present the same underlying datum, build them off one
> canonical aggregate; where two bases must coexist (a cheap rollup for a long
> index, the granular ledger for a chart), pin the invariant that keeps them equal
> with a test, so they reconcile by construction rather than by luck.

## The three bases

Token and cost data is aggregated from exactly three places.

1. **The ledger: `usage_events`.** The granular, per-event record. One row per
   priced (or unpriced) usage event, carrying the four token classes (input,
   output, cache read, cache write), a cost, an `occurred_at`, and the dedup keys
   that make a replayed line idempotent. Summing it is the source of truth.

2. **The session rollups: `sessions.total_*`.** Per-session running totals
   (`total_input_tokens`, `total_output_tokens`, `total_cache_read_tokens`,
   `total_cache_write_tokens`, `total_cost_usd`, plus `message_count` and the
   generated `total_tokens`). They are written by each session's rebuild so a
   long list or index never has to scan the ledger.

3. **The daily rollup: `session_usage_daily`.** Per (session, UTC day, model)
   sums of the four token classes and cost, plus an `unpriced` flag that
   reproduces the ledger's cost-incomplete predicate. Written by the same
   rebuild transaction (`internal/server/store/rollups.go`), it is the base the
   Insights money panels read (fleet mix, economics, cache savings, subagent
   cost share) so a page render never groups the ledger. An undated event folds
   into a NULL day, keeping the undated gap identical to the session rollups'.
   It is one of five insights rollup tables; the other four
   (`session_tool_rollup`, `session_file_churn`, `session_turns`,
   `session_activity_hourly`) carry no token or cost data but follow the same
   discipline: derived in the rebuild transaction, keyed on session_id only,
   pinned to their source rows by `TestRollupsDerivedInRebuild` and to the
   migration backfill by `TestRollupBackfillMatchesDerivations`
   (docs/insights-rollups.md).

## The load-bearing invariant

A session's projection is only ever written by a whole-session rebuild
(`store.RebuildSession`), which computes the ledger and the rollups from one
in-memory fold: usage events dedup in memory, and `sessions.total_*` is summed
from the exact row set the rebuild writes, in the same transaction. So, for
every session:

> `sessions.total_<class> == sum(usage_events.<class>_tokens)` for each of the four
> classes, `sessions.total_cost_usd == sum(usage_events.cost_usd)`, and
> `sessions.message_count == count(messages)`.

This holds by construction, but nothing in the schema enforces it, so it is exactly
the kind of thing that rots. It is pinned directly by
`TestSessionRollupMatchesLedger` (after live ingest and after an epoch rebuild,
across multiple agents, models, cache tokens, duplicate usage, undated usage, and
unpriced usage) and, for the specific Claude duplicate-usage case, by
`TestClaudeDuplicateUsageCountedOnce` in the parse package.

`sessions.model_fallback_count` follows the same construction against the
`model_fallbacks` table: the fold merges the several transcript lines of one
fallback (they share a dedup key) into one row in memory and counts the merged
set. The invariant `sessions.model_fallback_count == count(model_fallbacks)` is
pinned across ingest and rebuild by `TestClaudeModelFallbackMergesAndCounts` in
the parse package. The declined attempt's token counts live on the `model_fallbacks` row
only; they are deliberately NOT folded into `sessions.total_*` or `usage_events`
(whether a declined attempt is billed depends on where in the stream it declined, so
the totals stay a record of served usage).

## The one legitimate gap

The analytics surfaces filter `occurred_at IS NOT NULL`: an undated event has no day
to plot, so counting it in a headline but not in the daily chart would make the total
exceed the sum of the chart (the exact drift #40 fixed). The rollups carry no such
filter; they count every surviving event. So the rollup base and the ledger-analytics
base differ by exactly the undated usage, and by nothing else. In practice that is
zero (Claude, Codex, and pi all stamp the turn a usage line belongs to, so a NULL
`occurred_at` is a malformed transcript to fix at ingest, not usage to scatter across
the dashboard), but it is a real difference and is pinned to exactly the undated
amount by `TestUndatedUsageIsTheOnlyRollupAnalyticsGap`.

## Whole-day windows on the daily rollup

`session_usage_daily` is day-grained, so the Insights reads over it window in
whole UTC days (`AnalyticsFilter.clauseForRollupDay`). The upper bound is exact:
Insights pins `Until` to a bucket boundary (a UTC midnight) before any rollup
query runs. The lower bound is deliberately wider: a mid-day `Since` (the "now
minus N days" ranges) counts the window's first UTC day in full, where the
ledger scan cut it at the instant. The charts drew that first-day bucket either
way, so the rollup read fills it as a complete bucket instead of a partially
counted one. `TestUsageTrendsWholeDayWindow` pins the behavior so it stays a
decision rather than drifting. The ledger surfaces (`Analytics`, `cacheStats`,
the sparklines) keep their instant-precise windows, which is why they stay on
the ledger.

## Where each view reads, and how it reconciles

| View | Function | Base | Reconciliation |
| --- | --- | --- | --- |
| Overview usage panel (totals, daily grid, by-model, by-agent) | `Store.Analytics(0, …)` | ledger | One base grouped three ways; headline summed from the by-agent split, so `sum(by-model) == sum(by-agent) == headline` by construction (#40). |
| Project usage panel | `Store.Analytics(projectID, …)` | ledger | Same function, scoped to one project. The project header shows no rollup figure of its own (`Store.Project` loads identity only), so nothing on the page contradicts the panel. |
| Project sparklines (30d trend on the projects index) | `Store.ProjectSparklines` | ledger | Per-project daily cost over a trailing window; a trend, not a lifetime total, so it is not expected to equal the index's lifetime columns. |
| Projects index (tokens, cost columns) | `Store.ListProjects` | rollups | Lifetime per-project totals. Must equal the project usage panel's all-time figure (same datum, two pages). Pinned by `TestProjectsIndexReconcilesWithAnalytics`. |
| Global session list / project session list / subagents | `Store.ListAllSessions` / `Store.ListSessions` / `Store.Subagents` | rollups | Per-session rollups; the `tokens` sort walks the generated `total_tokens` column. |
| Session detail header (Tokens tile, cost) | `Store.SessionDetailByID` | rollups | Per-session rollups. The session page shows no ledger-derived figure beside them, so the invariant alone keeps them honest. |
| Insights fleet mix, economics, cache savings, subagent cost share | `fleetMixFrom`, `economicsFrom`, `cacheSavingsTrend`, `subagentTrendsFrom` | daily rollup | Per-day-and-model sums the trend grid re-buckets; pinned to the ledger by `TestRollupsDerivedInRebuild` and windowed in whole days (see above). |

### Why the index stays on rollups

Converging the projects index onto the ledger would mean a `GROUP BY` over
`usage_events` for every project on a list that should be a cheap rollup read. That
is the case issue #41 explicitly carves out: where a view genuinely needs the cheaper
rollup and another needs the ledger, keep both and pin the invariant with a test
rather than forcing one base. That is what `TestProjectsIndexReconcilesWithAnalytics`
does.

## When you add a view

If you add a surface that shows a token or cost figure:

1. Read from the base the table above uses for that cluster. Do not introduce a third
   query that sums the same datum a new way.
2. If your view genuinely needs the other base (a cheap index that should not scan the
   ledger, say), add or extend a reconciliation test so the two bases are pinned equal,
   the way `TestProjectsIndexReconcilesWithAnalytics` pins the index against the panel.
3. If you add a fifth token class or a new usage column, thread it through the
   rebuild fold (`store.RebuildSession`) and every aggregate query, and extend the
   invariant test. Dropping a class from one side is the regression these tests
   and the `projection-consistency` bastion reviewer exist to catch.
