# Agent notes for akari

Short orientation for coding agents. The full story lives in
[docs/development.md](docs/development.md) and [DESIGN.md](DESIGN.md); this file
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
`grace` (admin) with password `akari-dev`. See
[docs/development.md](docs/development.md) "Worktree-based development with eph"
and "Example data for development".

## Signals and the reparse epoch

Per-session signals (outcome, quality score and grade, tool health, prompt
hygiene, context health, observed thinking) live in `session_signals`, derived
from the projection off the hot path: the settle pass materializes a row once a
session has idled past the abandoned threshold, and a reparse re-derives it. The
append path deliberately does not compute signals (doing it per message is
quadratic in a session's turns, and a mid-session verdict drifts once the session
settles).

Five version constants gate the machinery, and forgetting one leaves a
half-migrated corpus: `parse.Version` and `parse.Epoch`, `quality.Version`,
`quality.PromptFactsVersion`, and `pricing.Version`. A sixth,
`quality.ThinkingScaleVersion`, marks the observed-thinking calibration but does
not gate. When you change parser or reducer output, the signal set or scoring,
prompt classification, the pricing table, or the thinking calibration, bump the
matching constant (and pair it with a `parse.Epoch` reparse where that constant's
rule says to).

The full rules live in [docs/signals.md](docs/signals.md): which constant covers
what and how each backfills, why a new signal must default to "unmeasured", the
absolute token scale observed thinking bands on, and the known per-row Claude
limitation that defers the fleet view (issue #98). Read it before you touch
signals, scoring, or pricing.
