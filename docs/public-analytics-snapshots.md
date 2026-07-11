# Public analytics snapshots

The published user (`/u/<username>`) and project (`/p/<id>`) overview pages
serve their aggregate panels from a process-local snapshot cache. Browser
caching cannot absorb this traffic because these revocable pages use
`Cache-Control: no-store`.

## Freshness and failure behavior

Each cache key identifies one user or project and one normalized date range.
A completed generation is immutable. Requests use it for one minute by default.
The first request after that freshness interval waits for a refresh; concurrent
requests for the same key wait on the same singleflight result. A refresh has a
two-minute timeout detached from any one waiting request, so canceling a browser
request does not cancel work that other waiters still need.

If a refresh fails, the previous generation may serve for 15 additional minutes
by default. Once that stale interval expires, the request fails instead of
serving an older result. Cold-start failures also fail because no prior generation
exists. Refresh errors are logged with the snapshot key.

The cache holds at most 256 generations by default and evicts the least recently
used entry. These environment variables change the policy:

| Variable | Default | Meaning |
| --- | --- | --- |
| `AKARI_ANALYTICS_SNAPSHOT_FRESHNESS` | `1m` | Fresh lifetime before the next request refreshes the key. Must be positive. |
| `AKARI_ANALYTICS_SNAPSHOT_STALE_FOR` | `15m` | Additional stale-on-error lifetime. Set `0` to disable stale serving. |
| `AKARI_ANALYTICS_SNAPSHOT_LIMIT` | `256` | Maximum completed generations held by one server process. Must be positive. |

The cache is process-local. Replicas coalesce and retain their own generations;
the weighted request budget owns cross-request admission within each process.

## Authorization and sharing

The publication lookup runs before every public cache lookup. A cached generation
never acts as authorization, and public responses remain `no-store`. Publishing
or unpublishing a user or project invalidates all of that scope's ranges and
prevents an already-running refresh from reinstalling its generation. A request
that starts after revocation therefore returns 404 without consulting the cache.
Completing a fleet reparse clears every generation so post-reparse requests cannot
reuse aggregates from the previous parse epoch.

Authenticated views reuse the same completed generation when their data shape is
identical:

- `/overview` reuses a published-user-shaped generation when exactly one user is
  selected.
- An unfiltered `/projects/<id>` view reuses the project generation. Filtered
  project views continue to read their narrower scope directly.

The project generation includes the by-user and trend fields needed by the
authenticated page. The public template does not render account names or session
links. On the authenticated page, its aggregate panel can trail the live session
rows by the configured freshness interval.

## Observability

Responses served through this cache include two headers:

- `X-Akari-Analytics-Snapshot` reports `state` (`hit`, `miss`, `refresh`, or
  `stale`), age in seconds, and the configured fresh and stale intervals.
- `Server-Timing` reports the snapshot lookup/refresh duration and snapshot age.

A stale-on-error response also carries HTTP `Warning: 110 - "Response is stale"`.
These headers make cold starts, refreshes, and stale fallback visible in browser
developer tools and load tests without making the response cacheable.

## Rollup and admission boundaries

Snapshot refreshes call the existing analytics and Insights store paths. They do
not introduce another rollup table. `session_usage_daily` and the other Insights
rollups remain owned by the rebuild pipeline, while issue #7 owns converting the
remaining ledger-backed `Analytics` reads. This cache limits repeated request-time
work and memory; it does not redefine the aggregation base.

The refresh is centralized in `Server.computeAnalyticsSnapshot`. The global
weighted request budget from issue #136 wraps that compute boundary, so cache hits
consume no admission capacity and one coalesced refresh accounts for the expensive
work once.
