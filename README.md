# akari

akari collects the local session logs of coding agents (Claude Code, Codex, and
pi), parses them on the server, and shows them in one place: a searchable history
of every session across your machines, grouped by the git project they ran in,
with token usage and cost. Sessions can be published for logged-out viewing.

It is an explicit client/server split. Many thin clients push raw session bytes
to one server; the server does all the parsing, storage, and rendering. The
client keeps no derived state, so a parser improvement reaches old sessions by
re-parsing on the server, with nothing re-uploaded. That reparse is automatic: a
new server binary notices its parser changed and rebuilds the stored projection in
the background, so there is no manual step after a parser upgrade.

## How it fits together

- **Clients** discover agent session files on disk, resolve each session's
  working directory to a canonical git remote, and stream the raw bytes to the
  server with a resumable, append-only protocol. A client runs anywhere; only the
  server is Linux-only.
- **The server** stores the raw bytes, parses them into a normalized projection
  (messages, tool calls, token usage), prices usage from a compiled-in rate
  table, and serves a web UI. Bulky tool bodies go into a content-addressed store
  (Postgres large objects), deduped across sessions. From the projection it also
  derives per-session signals (an outcome, a quality grade, and input and load
  measures) that feed the Insights analytics.
- **Projects** are keyed by canonical git remote, so the same repo cloned into
  several worktrees or machines collapses into one project.

```
  agent logs            akari client                 akari server
 (claude/codex/pi)  ──►  discover + resolve   ──►   ingest ─► parse ─► Postgres
                         (git remote)               raw bytes   projection + CAS
                                                                      │
                                                              web UI (templ+htmx)
```

## Install

Prebuilt, checksum-verified binaries are published for each release. Each script
downloads the archive for your OS and architecture, verifies it against the
release `SHA256SUMS`, and installs the binary. Set `AKARI_VERSION` (for example
`v0.1.0`) to pin a version instead of taking the latest.

Client, Linux and macOS:

```sh
curl -fsSL https://raw.githubusercontent.com/jssblck/akari/main/scripts/install.sh | sh
```

Client, Windows (PowerShell):

```powershell
irm https://raw.githubusercontent.com/jssblck/akari/main/scripts/install.ps1 | iex
```

Server, Linux:

```sh
curl -fsSL https://raw.githubusercontent.com/jssblck/akari/main/scripts/install-server.sh | sh
```

Add `-s -- --systemd` to the server command to also install a managed systemd
service, a dedicated `akari` user, and an environment file at
`/etc/akari/server.env`. See [docs/releases.md](docs/releases.md) for the asset
list and the install options.

### Updating

Both binaries update themselves to the latest release:

```sh
akari update            # update the client in place
akari update --check    # report whether an update is available, install nothing
akari-server update     # update the server in place
```

`akari update` is a native updater: it downloads the latest release archive for
your platform, verifies it against the release `SHA256SUMS`, and swaps the
binary in place with no shell or `curl` needed. On Windows it replaces the
running executable by moving it aside, so the update succeeds while akari is
running; restart any `akari watch`/`daemon` to pick up the new version.
`akari-server update` reuses the install script, then reminds you to restart the
service (`systemctl restart akari-server`) when one is installed. Inside a
container, rebuild the image and redeploy instead of updating the binary in
place; the server warns when it detects it is running in one.

## Running the server

The server is a container workload configured from the environment. The included
`docker-compose.yml` brings up Postgres and the server together:

```sh
docker compose up -d --build
```

It listens on `:8080` by default. The first account you register becomes the
admin and needs no invite; every later account needs an invite token an admin
mints from the account page. Registration is otherwise closed.

### Worktree-based development with eph

For day-to-day development across multiple git worktrees, use
[eph](https://github.com/attunehq/doteph) instead of docker-compose. The bundled
[`.eph`](.eph) file gives each worktree its own Postgres and its own
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

#### One-shot launch (preview/debug)

The bundled [`.claude/launch.json`](.claude/launch.json) starts the whole stack
in one action through `eph dev`. A Claude Desktop preview server runs a single
foreground command and offers no separate setup or teardown hook, so `eph dev`
fills both ends: it brings every service up and runs each `post-start` hook (the
same idempotent seed described below), foregrounds the server `run=` service on
the port the launcher assigns (passed as `$PORT`), and runs `eph down` when the
launch ends. `eph down` keeps the `pgdata` volume, so the next launch restarts
fast and stays seeded. Pass `--clean` (`runtimeArgs: ["dev", "--clean"]`) to
reset the volume on every launch instead. It is meant for the launch config; the
`eph up` loop above remains the way to drive the stack by hand.

### Example data for development

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

### Server configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `AKARI_DATABASE_URL` | (required) | Postgres connection string. |
| `AKARI_LISTEN` | `:8080` | Address the HTTP server binds. |
| `AKARI_COOKIE_INSECURE` | unset | Set truthy to drop the `Secure` flag on session cookies for plain-HTTP local development. |
| `AKARI_PUBLIC_URL` | (derived) | The externally reachable base URL (`https://akari.example.com`), used as the OAuth issuer and the base of every URL the MCP authorization flow advertises. Falls back to `AKARI_URL`; when neither is set the server derives the origin from each request (correct for a single-origin deployment behind a sane proxy). |
| `AKARI_SWEEP_INTERVAL` | `1h` | How often the server reclaims orphaned CAS blobs. A Go duration (`30m`, `2h`); `0` disables the background sweep. |
| `AKARI_OG_CACHE_TTL` | `1h` | How long a rendered Open Graph preview card is served from cache before the next request re-renders it. A Go duration; must be positive (the card is always cached). |
| `AKARI_OG_CLEANUP_INTERVAL` | `24h` | How often the server prunes expired preview cards (older than `AKARI_OG_CACHE_TTL`) from the cache. A Go duration; `0` disables the sweep. |
| `AKARI_SIGNALS_SETTLE_INTERVAL` | `5m` | How often the server computes per-session quality signals (outcome, grade, prompt hygiene, context health) for sessions that have settled: a session is graded once it has been idle past the abandoned threshold, off the ingest path, so a live session is never graded with an outcome that would drift. A Go duration; `0` disables the background pass (signals then land only on reparse or via `akari-server settle`). |

Migrations are embedded and applied on startup, so the server is safe to restart.

### Maintenance subcommands

```sh
akari-server            # run the HTTP server (default)
akari-server reparse    # force a rebuild of every projection from stored raw bytes
akari-server reparse --agent claude   # limit a reparse to one agent
akari-server sweep      # reclaim orphaned CAS blobs now
akari-server settle     # compute quality signals for every settled session now
akari-server dev-seed   # fill a local server with example data (see Example data above)
akari-server update     # update to the latest release (see Updating below)
akari-server version    # print the build version and exit
```

The server reparses on its own when its parser changes: it compares a compiled-in
parser epoch against the epoch the stored data was last rebuilt under and, when they
differ, reparses in the background on startup. An admin can also force one from the
account page. `akari-server reparse` is the manual escape hatch and forces a run
regardless of the epoch; it sweeps orphaned blobs afterward, as the automatic run
does. `sweep` is the manual form of the periodic background sweep, and `settle` is
the manual form of the periodic signals pass (it grades every settled session that
is missing a current-version row, then exits).

## Running a client

Install the client (see [Install](#install) above) or build it from source, then
point it at your server:

```sh
go build -o akari ./cmd/akari

akari login --server https://akari.example.com --token <ingest-token>
```

Create the ingest token from the server's account page (the `ingest` scope is
push-only). The client writes its config to the OS config directory.

Then push your sessions:

```sh
akari sync                 # one-shot: scan and upload everything new
akari sync --dry-run       # show what would upload, with skip reasons
akari sync --time-limit 30s  # upload for up to 30s, finish the in-flight file, then exit
akari watch                # stay running, upload sessions as they change
akari daemon start         # run watch in the background (per-OS)
akari daemon status
akari daemon stop
akari update               # update to the latest release (see Updating below)
akari version              # print the build version and exit
```

`akari sync` stops starting new uploads after `--time-limit`, a Go duration such
as `30s` or `5m` (default 5 minutes; `0` removes the cap). The limit gates only when new work begins. The
file being uploaded when the limit elapses runs to a clean stopping point, so a run
can finish a little past the limit but never abandons an upload mid-stream. Because
uploads resume from the server's cursor, repeated short runs ingest a backlog in
chunks. That is handy for trickling in data, or for grabbing a few seconds of
sample sessions while a dev server is up.

The client discovers Claude, Codex, and pi sessions in their standard locations.
A session whose working directory is not a git repository is skipped with a
warning rather than uploaded under an ambiguous project.

## The web UI

A persistent left sidebar carries the primary sections (Overview, Insights,
Projects, Sessions, Account); the signed-in user and log-out sit at its foot.

- **Overview**: the landing surface. A fleet-wide usage panel bounded to a
  trailing window (7, 30, or 90 days, a year, or all of history): cost, combined
  tokens, and session totals, a daily-activity heatmap, and a by-model and
  by-agent breakdown, every figure scoped to the chosen window.
- **Insights**: the cross-cutting analytics surface, scoped to the same trailing
  window but reading how sessions went rather than what they cost. Headline bands
  for concurrency (peak overlap, busiest user, average) and velocity (turn-cycle
  latency percentiles and throughput); a tools band (call volume, error rate, and
  the busiest tools banded by reliability); a prompt-hygiene band (how clearly the
  window's prompts set the agent up); a context-health band (how heavy sessions
  got and how often they shed context); distributions of quality grades, outcomes,
  and session archetypes; and a file-churn list of paths edited more than once.
- **Sessions**: every session across all projects in one place, with a faceted
  filter rail (agent, project, user, and machine, each with counts) and a project
  column, so a run is findable without first choosing its project.
- **Projects index**: one full-width table of git-remote projects, each row with
  its session count, a single token total (hover it for the in/out/cache-read/
  cache-write breakdown), cost, a 30-day cost sparkline, and a relative "updated"
  time. Fleet usage lives on the Overview; local folders reach you through the
  Sessions filter rail, so neither crowds this surface.
- **Project view**: that project's sessions across all users and machines, with
  agent, user, and machine filters, and the same analytics panel scoped to the
  project.
- **Session view**: a sticky stats header (tokens in/out/cache, cost, duration,
  message counts, and a Quality tile carrying the session's grade and outcome) and
  the transcript: messages, thinking, and tool calls, with a timeline rail that
  maps the turns and flags errored tools. The Quality and Tokens tiles reveal
  their drivers on hover: the tool-health counts and prompt-hygiene flags behind
  the grade, and the peak context and reset count behind the tokens. Tool input
  and result bodies show as size/type chips that expand inline on click, fetched
  from the CAS; an editing tool's input expands as a rendered diff. A density
  toggle switches between a comfortable and a compact reading mode. Subagent
  sessions are listed under the session that spawned them. In-progress sessions
  update live over server-sent events.
- **Charts** are rendered by a small dependency-free SVG module bundled as a
  static asset; the UI fonts (Geist and Geist Mono) are self-hosted, so the binary
  stays self-contained with no Node toolchain.
- **Account**: API tokens (ingest, read, or full scope), connected MCP apps (with
  a one-click disconnect that revokes their tokens), invites for admins, and a
  Publicity control to publish your own usage overview (see below).

### Session quality and insights

Every session carries derived signals computed from its own projection, never
from a label the agent emits. An outcome (completed, abandoned, errored, or
unknown, each with a confidence) is inferred from the last substantive turn,
unresolved tool calls, and any trailing failures. A 0-100 quality score and an
A-F grade come from a penalty model over tool health: failures, immediate
retries, edit churn, and the longest failure streak, weighed with the outcome.
Two further signal sets are informational and never move the grade. Prompt
hygiene reads the human's input (terse, repeated, or no-code-context prompts, and
unstructured openings); context health reads resource load (the heaviest
single-turn context a session held, as a raw token count independent of any
model's window, and the number of inferred context resets, the sharp drops that
read as a compaction or a clear).

The signals live in their own table, rebuilt from the session's stored messages,
tool calls, and usage on catch-up or reparse. They sit outside the token-rollup
invariant, so `sessions.total_*` still equals the sum over `usage_events`. A signals version stamps each row: the analytics
count only rows at the current version, and a fleet-wide reparse epoch rebuilds
the whole corpus when the model changes, so a scoring change reaches old sessions
on the next deploy without re-uploading anything.

### Visibility and publishing

Sessions are `internal` (visible to any logged-in user) by default. There is no
private-to-one-user state, by design: logged-in means you see everything. The
owner of a session can publish it, which mints an unguessable link at
`/s/{public_id}` for logged-out viewing; unpublishing clears the link so it stops
resolving. A public page never exposes the numeric session id, and a published
session only links to subagents that are themselves public.

A user can also publish their own usage overview from the account page's
Publicity section, which exposes it at `/u/<username>` for logged-out viewing.
The public page is the same aggregate panel the owner sees (totals, the activity
grid, and the by-model and by-agent breakdowns), scoped to that one account: it
carries no session links and no other user's usage, so publishing the overview
shares neither sessions nor anyone else's numbers. The address is the username, so
making it private hides the page without changing the link, and re-publishing
brings the same URL back. When public, a badge on the owner's overview links to
the page.

A published overview also gets an Open Graph preview card at
`/u/<username>/og.png`, so a shared link unfurls with an image: a simplified copy
of the activity heatmap plus the total-token and session figures, rendered in the
house style. The card is a pure-Go PNG (no headless browser), rendered on demand
the first time it is fetched (a share unfurl) and cached for a short TTL
(`AKARI_OG_CACHE_TTL`); once it expires the next fetch renders a fresh one, so the
preview tracks the account's usage without re-rendering on every crawl and without
rendering at all for an overview nobody shares. A background sweep
(`AKARI_OG_CLEANUP_INTERVAL`) prunes expired cards from the cache.

CAS blobs are served per session, not by bare hash: a viewer can fetch a tool
body only through a session that references it and that they may see. This keeps
the cross-session dedup from leaking an internal body through a public link.

### Retention

The owner of a session (or an admin) can delete it from the session page.
Deleting cascades its transcript and raw bytes; any CAS blobs it referenced are
reclaimed by the next sweep.

## The MCP server

akari serves a remote [Model Context Protocol](https://modelcontextprotocol.io)
endpoint at `/mcp`, so a coding agent can read your whole session history without
opening the UI. It exposes the same surface the web UI shows (the overview
analytics, the projects index, the session feed, a session's full transcript) plus
the raw underlying data behind it: tool-call bodies from the content store, and the
lossless bytes a session was ingested from. It is read-only.

Connect it once from your harness. In Claude Code:

```sh
claude mcp add --transport http akari https://akari.example.com/mcp
```

On first use the harness opens your browser to akari, which recognizes the session
you are already signed in to and asks you to approve the connection. Nothing is
copied into the agent and no password is typed into it: the browser sign-in is the
authentication, and approving is one click. Behind that click is the OAuth 2.1 flow
MCP defines, with akari acting as both the resource and the authorization server.
The agent registers itself, redirects through a PKCE-protected authorization
request, and exchanges the result for a read-only access token that refreshes on
its own. Disconnect a connected app from the account page at any time; that revokes
its tokens at once.

For the flow to advertise correct URLs behind a proxy, set `AKARI_PUBLIC_URL` to
the server's external origin (see [Server configuration](#server-configuration)).

### Tools

Every tool is read-only and sees every internal session, the same surface a
logged-in user sees.

| Tool | Returns |
| --- | --- |
| `whoami` | The account the credential authenticates as. |
| `overview` | Fleet usage for a trailing window: cost, tokens by class, session count, a daily series, and by-model and by-agent breakdowns. |
| `list_projects` | Every project, most recently active first, with session counts and token and cost totals. |
| `get_project` | One project's identity, its windowed analytics (optionally narrowed by agent, user, or machine), and the agents, users, and machines that ran in it. |
| `list_sessions` | The cross-project session feed with filters and sortable columns, plus the facet rail of busiest agents, users, machines, and projects. |
| `get_session` | One session's header and full transcript: messages, thinking, tool-call metadata, attachments, and subagents. |
| `read_tool_body` | A tool call's input or result body from the content store, by the hash a tool call carries. |
| `get_session_raw` | The lossless bytes a session was ingested from, behind the parsed projection. |

### Connecting without a browser

A harness that cannot run the browser flow can authenticate with a read-scope API
token instead. Create one on the account page (the `read` scope is read-only, the
counterpart of the push-only `ingest` and the read-write `full`) and pass it as a
bearer token:

```sh
claude mcp add --transport http akari https://akari.example.com/mcp \
  --header "Authorization: Bearer <read-token>"
```

A read token reaches only the MCP endpoint: it cannot push sessions or drive the
write surface.

## Development

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

### Layout

- `cmd/akari-server` is the server entry point (plus `reparse` and `sweep`).
- `cmd/akari` is the client CLI (`login`, `sync`, `watch`, `daemon`).
- `internal/parser` holds the per-agent parsers and their fixtures.
- `internal/pricing` is the compiled-in model rate table.
- `internal/server` is the data layer, HTTP surface, parse pipeline, web UI, and
  the remote MCP server (`internal/server/mcpserver`, with its OAuth flow in
  `internal/server/httpapi`).
- `internal/client` is discovery, git remote resolution, the upload protocol,
  and the watch/daemon machinery.
- `migrations` holds the embedded SQL schema.

See `docs/DESIGN.md` for the full engineering design and rationale, and
`DESIGN.md` for the visual design system.

## Releases

Releases are cut by pushing a `vX.Y.Z` tag. CI cross-compiles the server (Linux)
and the client (Linux, macOS, Windows), packages each target into an archive with
a `SHA256SUMS`, and publishes a GitHub Release with notes generated from the
merged pull requests. The same build runs as a dry run on every pull request and
`main` push, so a break in the release pipeline surfaces on the PR. The binaries
report the tag through `akari version` / `akari-server version`. See
[docs/releases.md](docs/releases.md) for the full process and asset list.
