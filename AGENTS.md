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
