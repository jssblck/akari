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

## Parsing, signals, and the parse epoch

Parsing is rebuild-on-dirty: the ingest path only appends raw bytes, and a
background worker rebuilds a session's whole projection whenever its raw bytes
or the parser have moved (docs/DESIGN.md, "Server-side parsing pipeline").
There is no incremental parse; never write projection rows outside the rebuild.
Per-session signals (outcome, quality score and grade, tool health, prompt
hygiene, context health, observed thinking) live in `session_signals`, graded
off the hot path once a session settles or is declared terminal (a mid-session
verdict would drift).

One constant gates every derived representation: `parse.Epoch`. Bump it in the
same commit as any change to parser or reducer output, a rebuild-derived
column, the signal set or scoring, prompt classification, the pricing table, or
the thinking calibration's stored scalars; the next deploy rebuilds the corpus
in the background. The golden-fixtures test fails by name when you forget.
[docs/signals.md](docs/signals.md) has the full rules, including why a new
signal must default to "unmeasured" and the absolute token scale observed
thinking bands on. Read it before you touch signals, scoring, or pricing.
