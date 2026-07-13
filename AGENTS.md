# Agent notes for akari

Short orientation for coding agents. The full story lives in
[docs/development.md](docs/development.md) and [DESIGN.md](DESIGN.md); this file
just front-loads the three things that bite if you skip them.

## Build both frontend layers

The application UI lives in `frontend/` and builds into
`internal/server/frontend/dist/`, which Go embeds in the server binary. The root
homepage remains templated under `internal/server/web/`. Rebuild both layers
through the Makefile so the committed React artifact and generated Go stay in
step:

```sh
make build          # build React, generate templ, then compile Go
make test           # check React, rebuild it, then run Go tests under -race
make frontend-check # Biome and TypeScript only
go generate ./...   # regenerate the templated homepage only
```

The production frontend artifact is committed so release cross-compilation and
downstream source builds still require only Go. Run `make frontend` after any
file under `frontend/` and commit the resulting `dist/` changes.

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
