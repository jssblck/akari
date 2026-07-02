# Self-hosting

The akari server is a single Linux binary backed by Postgres. It embeds its own
UI, fonts, and database migrations. This chapter covers standing it up,
configuring it, and the operations it needs over its life. (The client that pushes
sessions to it runs anywhere and is [its own chapter](./the-client.md).)

## Running the server

### With Docker Compose

The bundled `docker-compose.yml` brings up Postgres and the server together and is
the quickest way to a running instance:

```sh
docker compose up -d --build
```

It starts Postgres 18 and the server, which applies its migrations on startup and
listens on `:8080`. The compose file runs in plain-HTTP development mode
(`AKARI_COOKIE_INSECURE=1`) and ships throwaway database credentials; change both
before exposing it. For anything real, terminate TLS at a reverse proxy in front
(see [Production](#production)) and point the server at a Postgres you manage
rather than the bundled container.

### With the install script (systemd)

On a Linux host, the server install script downloads a checksum-verified binary
and, with `--systemd`, wires it up as a managed service:

```sh
curl -fsSL https://raw.githubusercontent.com/jssblck/akari/main/scripts/install-server.sh | sh -s -- --systemd
```

That installs a dedicated `akari` user, a `akari-server` systemd service, and an
environment file at `/etc/akari/server.env` where you set the configuration below.
Manage it the usual way:

```sh
sudo systemctl start akari-server
sudo systemctl status akari-server
sudo systemctl restart akari-server
```

### From source

With a Go toolchain and a reachable Postgres:

```sh
go generate ./...                         # regenerate the templ UI (gitignored)
go build -o akari-server ./cmd/akari-server
export AKARI_DATABASE_URL="postgres://akari:akari@localhost:5432/akari?sslmode=disable"
./akari-server
```

The server applies its embedded migrations on startup, so there is no separate
migration step, and a restart is always safe.

## Configuration

The server is configured entirely from the environment; there is no config file.
Only the database URL is required.

| Variable | Default | Meaning |
| --- | --- | --- |
| `AKARI_DATABASE_URL` | (required) | Postgres connection string, for example `postgres://akari:akari@localhost:5432/akari?sslmode=disable`. |
| `AKARI_LISTEN` | `:8080` | Address the HTTP server binds. Falls back to `PORT` when unset. |
| `AKARI_PUBLIC_URL` | (derived) | The externally reachable base URL (`https://akari.example.com`), used as the OAuth issuer and the base of the URLs the [MCP](./agent-access.md) authorization flow advertises. Falls back to `AKARI_URL`; when neither is set the server derives the origin per request, which is correct for a single-origin deployment behind a proxy that forwards the host. |
| `AKARI_COOKIE_INSECURE` | unset | Set truthy to drop the `Secure` flag on session cookies, for plain-HTTP local development. Leave unset in production so cookies are HTTPS-only. |
| `AKARI_SWEEP_INTERVAL` | `1h` | How often the server reclaims orphaned content-addressed blobs. A Go duration (`30m`, `2h`); `0` disables the background sweep. |
| `AKARI_OG_CACHE_TTL` | `1h` | How long a rendered Open Graph preview card of a published overview is served from cache before the next request re-renders it. A Go duration; must be positive. |
| `AKARI_OG_CLEANUP_INTERVAL` | `24h` | How often the server prunes expired preview cards (older than `AKARI_OG_CACHE_TTL`) from the cache. A Go duration; `0` disables the sweep. |
| `AKARI_SIGNALS_SETTLE_INTERVAL` | `5m` | How often the server computes per-session quality signals (outcome, grade, prompt hygiene, context health) for sessions that have settled: a session is graded once it has been idle past the abandoned threshold (30 minutes), off the ingest path, so a live session is never graded with a verdict that would drift. A Go duration; `0` disables the background pass (signals then land only on reparse or via `akari-server settle`). |

## The database

akari stores everything in Postgres: raw session bytes, the parsed projection, user
accounts, tokens and invites, and content-addressed blobs (as large objects).
Postgres 18 is what the compose file and CI use.

Migrations are embedded in the binary and applied on every startup: the server
records each applied migration and runs only the new ones, each in its own
transaction, so restarts and upgrades need no manual database step. Back it up like
any Postgres database on your normal schedule; a standard `pg_dump` that includes
large objects captures the blobs along with everything else.

## The first account

Registration is closed and invite-gated, with one bootstrap exception: **the first
account registered on a fresh server needs no invite and becomes the admin.** Open
the server in a browser and register to claim it. That account can then mint
invite tokens (Account page) for everyone
else, who redeem them when they register. The full account and token model is
[Accounts and sharing](./accounts-and-sharing.md).

## Reparse

The server keeps each session's raw bytes and a projection parsed out of them, and
can rebuild the projection from the bytes at any time (a **reparse**). It runs one
on its own when its parser changes: a new binary compares a compiled-in parser
epoch against the epoch the stored data was built under and, when they differ,
reparses in the background on startup while it keeps serving. There is no manual
step after a parser upgrade.

You can also force one:

- **From the Account page**, an admin can trigger a reparse and watch its progress
  on a live bar.
- **From the CLI:**

  ```sh
  akari-server reparse                 # rebuild every projection from stored raw bytes
  akari-server reparse --agent claude  # limit to one agent
  ```

While a reparse runs, the parsed pages show a progress notice instead of a
half-rebuilt view; the Account page and raw-byte reads stay available throughout. A
reparse sweeps orphaned blobs when it finishes.

## Maintenance subcommands

The server binary carries a few operational subcommands beside the default
run-the-server behavior:

```sh
akari-server                  # run the HTTP server (default)
akari-server reparse          # force a projection rebuild (see above)
akari-server sweep            # reclaim orphaned content-addressed blobs now
akari-server settle           # compute quality signals for every settled session now
akari-server dev-seed         # fill a local server with example data (development)
akari-server update           # update to the latest release in place
akari-server version          # print the build version and exit
```

`sweep` is the manual form of the periodic blob reclaim; it is safe to run any
time, since blob liveness is computed rather than reference-counted. `settle` is
the manual form of the periodic signals pass: it grades every settled session
missing a current-version signals row, then exits. `update`
downloads and swaps in the latest release (and reminds you to
`systemctl restart akari-server` when a service is installed); inside a container,
rebuild the image and redeploy rather than updating the binary in place.

`dev-seed` is a development convenience: it creates a few demo accounts (sign in as
`grace`, the admin, with password `akari-dev`) and ingests this machine's real
agent sessions. It is idempotent (a no-op once the store holds sessions) and
best-effort by default. Keep it away from
any server holding real data.

## Production

A short checklist for a real deployment:

- **Terminate TLS at a reverse proxy** (nginx, Caddy, and the like) in front of the
  server, which itself speaks plain HTTP. Forward to its `AKARI_LISTEN` address.
- **Set `AKARI_PUBLIC_URL`** to the external HTTPS origin so the MCP OAuth flow
  advertises correct URLs.
- **Leave `AKARI_COOKIE_INSECURE` unset** so session cookies are marked `Secure`
  and ride only over HTTPS.
- **Point `AKARI_DATABASE_URL` at a managed Postgres**, not the bundled container,
  and back it up on your normal schedule.
- **Capture logs** through your container runtime or systemd; the server logs to
  standard output and error.

The server shuts down gracefully on interrupt: it drains in-flight requests and
lets background work (sweep, card refresh, any reparse) wind down before the
connection pool closes.

---

Next: [Glossary](./glossary.md) -> the terms the guide uses, for reference.
