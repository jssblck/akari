# Agent notes for akari

Short orientation for coding agents. The full story lives in
[README.md](README.md) (see "Development") and [DESIGN.md](DESIGN.md); this file
just front-loads the three things that bite if you skip them.

## Generate templ before you build

The web UI is templ. The `.templ` files under `internal/server/web/` are the
source of truth; the `*_templ.go` they compile to are gitignored and not present
on a fresh clone. A bare `go build ./...` or `go test ./...` fails with
`undefined: web.*` until the generated code exists. Run the generate step first:

```sh
make build          # go generate ./... then go build ./...
make test           # go generate ./... then go test -race ./...
go generate ./...   # just regenerate after editing a *.templ
```

Re-run `go generate ./...` (or `make generate`) after editing any `*.templ`.

## Integration tests gate on a database

Tests that touch Postgres skip cleanly unless `AKARI_TEST_DATABASE_URL` is set.
A green `go test ./...` with the variable unset has silently skipped the store,
parse, and web integration tests. Under eph the variable is already set, so the
one-liner is:

```sh
eph run go test ./...
```

Without eph, point it at any Postgres whose role may create databases (each test
provisions and drops its own database beside the one the URL names):

```sh
AKARI_TEST_DATABASE_URL=postgres://akari:akari@localhost:5432/akari go test ./...
```

## Running the app locally

`eph up` brings up Postgres plus the server and seeds demo data; sign in as
`grace` (admin) with password `akari-dev`. See README "Worktree-based development
with eph" and "Example data for development".

## Signals and the reparse epoch

Per-session signals (outcome, quality score and grade, tool health, prompt
hygiene, context health) live in `session_signals`, derived from the projection.
A row is materialized by the settle pass (`RefreshSettledSignals`, run on a timer
from `cmd/akari-server` and available as `akari-server settle`) once the session
has been idle past the abandoned threshold, and re-derived on reparse. The ingest
append path deliberately does NOT compute signals: doing it per message would be
quadratic in a session's turns, and a verdict taken mid-session would drift once
the session crossed the idle threshold, so signals are computed once, after the
session settles, off the hot path. They sit OUTSIDE the `sessions.total_* ==
sum(usage_events)` invariant: a new signal derives from the same rows without
touching the rollups. Five version constants gate the machinery, and forgetting
one leaves a half-migrated corpus:

- `parse.Version` and `parse.Epoch` (`internal/server/parse`): bump both when
  parser or reducer output changes (a new or removed row, a changed field, a
  reprice). The golden-fixtures test (`internal/server/parse/epoch_test.go`)
  fails by name and tells you to bump the epoch and refresh the snapshots with
  `go test ./internal/server/parse -run TestGoldenProjection -update`.
- `quality.Version` (`internal/quality`): bump when the signal set or the scoring
  changes, so the analytics count only rebuilt rows. The settle pass treats a
  stale-version row as due and re-stamps it once the session is settled, so a
  version bump backfills incrementally on its own. The `Epoch` bump forces a
  one-time full reparse that re-derives every row atomically, the belt-and-suspenders
  path when you want the whole corpus current on first deploy regardless of whether
  the settle loop is enabled.
- `quality.PromptFactsVersion` (`internal/quality`): bump when `ClassifyPrompt`'s
  output changes (a new or changed prompt flag, a different duplicate-digest
  normalization). Unlike `quality.Version`, these facts are cached on the messages
  row and can only be re-derived by re-inserting the message, so the settle pass
  cannot re-derive them: `gatherPromptHygiene` treats an older-version row like an
  unfilled one and leaves the session ungraded until a reparse fills it. Pair the
  bump with a `parse.Epoch` bump so the corpus reparses and re-derives every
  message's facts at the new version.
- `pricing.Version` (`internal/pricing`): bump on any rate change in the pricing
  table (a new or removed model, a different Input/Output/CacheWrite/CacheRead
  number). It gates `reconcileCacheSavingsPricingIfNeeded`, which re-prices the
  per-session cache-savings rollup across the cache-bearing corpus once per bump,
  so a session whose reparse fails still re-prices from its usage_events at the new
  rates (per-row cost rides the `Epoch` reparse instead). It is separate from
  `parse.Epoch` on purpose: keying off Epoch would re-price the whole corpus on
  every unrelated parser change. Pair a `pricing.Version` bump with the
  `parse.Epoch` bump a reprice already requires.

A new signal should default to a value that reads as "unmeasured" (NULL, or a
zero the aggregate excludes) until the settle pass (or a backfill reparse) fills it.
