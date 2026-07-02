# Development

This page is the day-to-day development loop for akari: generating the templ
output, the Makefile and plain-Go workflows, the integration-test database
setup, running the stack under eph, and seeding example data. For a quick
orientation aimed at coding agents, read [AGENTS.md](../AGENTS.md) first.

The web UI is server-rendered with [templ](https://templ.guide). The `.templ`
files under `internal/server/web/` are the source of truth; the Go they compile
to (`*_templ.go`) is gitignored and regenerated on every build rather than
committed, so editing one page no longer collides with another on a regenerated
file. templ is pinned as a Go tool in `go.mod`, so no separate install is needed:
`go generate ./...` runs the right version.

A `Makefile` wraps the common tasks and regenerates the templ output first, so a
fresh clone is one command from a binary:

```sh
make build        # go generate ./... then go build ./...
make test         # go generate ./... then go test -race ./...
make generate     # regenerate templ output after editing a *.templ
make vet
make fmt          # report files that are not gofmt-clean
```

Without `make`, regenerate once after cloning (or after editing a template) and
then use the Go tools directly:

```sh
go generate ./...   # regenerate internal/server/web/*_templ.go (gitignored)
go build ./...      # compile everything
go vet ./...
go test ./...       # unit tests
```

Integration tests provision an isolated database per test: each test creates a
uniquely named database, migrates it, and drops it on cleanup (see
`internal/server/storetest`). Because no two tests share a database, the suite
runs at the default package parallelism (and individual tests run in parallel),
so there is no `-p 1`. Point `AKARI_TEST_DATABASE_URL` at any Postgres whose role
may create databases; only the host and credentials are used, since each test's
database is created beside the one the URL names (via the `postgres` maintenance
database), so that named database need not exist.

Under [eph](#worktree-based-development-with-eph) the variable is already set to
the workspace's Postgres, separate from the `akari` database the running server
uses, so the tests never disturb it:

```sh
eph run go test ./...
```

Without eph, point the variable at any Postgres you control:

```sh
AKARI_TEST_DATABASE_URL=postgres://akari:akari@localhost:5432/akari \
  go test ./...
```

Tests that need the database skip cleanly when `AKARI_TEST_DATABASE_URL` is unset.

## Worktree-based development with eph

For day-to-day development across multiple git worktrees, use
[eph](https://github.com/attunehq/doteph) instead of docker-compose. The bundled
[`.eph`](../.eph) file gives each worktree its own Postgres and its own
natively-run server, each on a random free host port, so two worktrees never
collide the way the fixed ports in `docker-compose.yml` would.

```sh
eph up                  # Postgres + server, each on its own host port
eph status              # show the assigned ports and the server URL
eval "$(eph env)"       # load AKARI_DATABASE_URL, AKARI_URL, etc. into the shell
eph down                # stop the stack (keeps data); eph clean drops the volume
```

The server runs as `go run ./cmd/akari-server`, so a restart picks up source
changes, and it applies its embedded migrations on boot. Point the client at the
URL `eph status` reports (also exported as `AKARI_URL`).

### One-shot launch (preview/debug)

The bundled [`.claude/launch.json`](../.claude/launch.json) starts the whole
stack in one action through `eph dev`. A Claude Desktop preview server runs a
single foreground command and offers no separate setup or teardown hook, so
`eph dev` fills both ends: it brings every service up and runs each `post-start`
hook (the same idempotent seed described below), foregrounds the server `run=`
service on the port the launcher assigns (passed as `$PORT`), and runs `eph down`
when the launch ends. `eph down` keeps the `pgdata` volume, so the next launch
restarts fast and stays seeded. Pass `--clean` (`runtimeArgs: ["dev", "--clean"]`)
to reset the volume on every launch instead. It is meant for the launch config;
the `eph up` loop above remains the way to drive the stack by hand.

## Example data for development

The `.eph` server service runs `akari-server dev-seed` as a post-start hook, so
the first `eph up` against an empty database leaves you with something to look at.
It creates a few demo accounts (all sharing the password `akari-dev`), then runs
the akari client in-process for 30 seconds to ingest *this machine's* real agent
sessions through the normal upload and parse pipeline, and finally reassigns those
sessions randomly across the accounts so the UI looks like a small team's history.
Sign in to the local UI as `grace` (the first roster account, which is the admin),
or as one of the other default handles `ada`, `anna`, or `katherine`.

It is idempotent: once the store holds sessions it is a no-op, so later `eph up`
runs cost nothing. The ingest is bounded by `--time-limit` (default 30s): when the
window elapses, in-flight uploads are cancelled rather than left to finish, so a
few very large local sessions cannot make the hook block `eph up`. To re-seed (or
run it by hand against a stack already up):

```sh
eph run go run ./cmd/akari-server dev-seed --force   # clear sessions, re-ingest, re-shuffle
go run ./cmd/akari-server dev-seed --users 6 --time-limit 1m   # more accounts, longer ingest
```

`--force` clears existing sessions before re-seeding. That clean slate matters:
the client keys a session on (its account, agent, source id), so re-ingesting
under the seed account after a prior run had moved sessions to other accounts
would otherwise create duplicate rows.

`dev-seed` is best-effort by default (it logs and exits 0 on failure so it never
blocks `eph up`); pass `--strict` to make failures non-zero when invoking it
yourself. It reads `AKARI_DATABASE_URL` and the upload target from `AKARI_URL`
(falling back to `--server-url` or `AKARI_LISTEN`).

## Layout

- `cmd/akari-server` is the server entry point, plus its maintenance
  subcommands (`reparse`, `sweep`, `settle`, `dev-seed`, `update`).
- `cmd/akari` is the client CLI (`login`, `sync`, `watch`, `daemon`, `update`).
- `internal/parser` holds the per-agent parsers and their fixtures.
- `internal/pricing` is the compiled-in model rate table.
- `internal/server` is the data layer, HTTP surface, parse pipeline, web UI, and
  the remote MCP server (`internal/server/mcpserver`, with its OAuth flow in
  `internal/server/httpapi`).
- `internal/client` is discovery, git remote resolution, the upload protocol,
  and the watch/daemon machinery.
- `migrations` holds the embedded SQL schema.

See [docs/DESIGN.md](./DESIGN.md) for the full engineering design and rationale,
[DESIGN.md](../DESIGN.md) for the visual design system,
[docs/releases.md](./releases.md) for the release process, and
[CONTRIBUTING.md](../CONTRIBUTING.md) for contribution expectations.
