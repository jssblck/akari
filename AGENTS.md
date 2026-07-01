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
hygiene, context health) live in `session_signals`, derived from the projection
and rebuilt on catch-up or reparse. They sit OUTSIDE the `sessions.total_* ==
sum(usage_events)` invariant: a new signal derives from the same rows without
touching the rollups. Three version constants gate the machinery, and forgetting
one leaves a half-migrated corpus:

- `parse.Version` and `parse.Epoch` (`internal/server/parse`): bump both when
  parser or reducer output changes (a new or removed row, a changed field, a
  reprice). The golden-fixtures test (`internal/server/parse/epoch_test.go`)
  fails by name and tells you to bump the epoch and refresh the snapshots with
  `go test ./internal/server/parse -run TestGoldenProjection -update`.
- `quality.Version` (`internal/quality`): bump when the signal set or the scoring
  changes, so the analytics count only rebuilt rows. The `Epoch` bump is what
  backfills the corpus, and a reparse re-stamps every row at the running version.

A new signal should default to a value that reads as "unmeasured" (NULL, or a
zero the aggregate excludes) until the backfill reparse fills it.
