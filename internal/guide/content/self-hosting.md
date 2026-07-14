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
| `AKARI_PUBLIC_URL` | (derived) | The externally reachable base URL (`https://akari.example.com`). It is the OAuth issuer, the base of the URLs the [MCP](./agent-access.md) authorization flow advertises, and the trusted origin for browser writes. It may carry a path (`https://ops.example.com/akari`) when a reverse proxy serves akari under a prefix; see [Serving under a path prefix](#serving-under-a-path-prefix). Query, fragment, and user information are rejected. When unset the server derives the origin per request, which suits local dev where the server is reached on more than one loopback port. Set it explicitly in production. |
| `AKARI_PREFIX_HEADER` | unset | The request header a trusted reverse proxy sets to the external path prefix it serves akari under (for example `X-Forwarded-Prefix`). When set, the prefix resolves per request from that header, so one instance can be mounted wherever the proxy chooses without a restart. Only safe when akari is reachable exclusively through the proxy; when `AKARI_PROXY_AUTH_SECRET` is set, the header is honored only on requests whose secret matches. See [Serving under a path prefix](#serving-under-a-path-prefix). |
| `AKARI_MCP_RESPONSE_BUDGET_BYTES` | `8388608` | Maximum encoded MCP tool result in bytes. Transcript pages stop at a message boundary before this limit; oversized message fields become authenticated content references. Must be between 8388608 (8 MiB) and 16777216 (16 MiB). |
| `AKARI_COOKIE_INSECURE` | unset | Set truthy to drop the `Secure` flag on session cookies, for plain-HTTP local development. Leave unset in production so cookies are HTTPS-only. |
| `AKARI_PROXY_AUTH_HEADER` | unset | Enables reverse-proxy single sign-on. The request header a trusted proxy in front sets to the authenticated username (for example `X-Auth-Request-Preferred-Username`). When set, akari trusts that header as the signed-in user and provisions the account on first sight. Leave unset for a direct, locally-authenticated deployment. See [Single sign-on behind a trusted proxy](#single-sign-on-behind-a-trusted-proxy). |
| `AKARI_PROXY_AUTH_SECRET` | unset | Optional shared secret the proxy must echo (in `AKARI_PROXY_AUTH_SECRET_HEADER`) for the identity header to be trusted. Defense in depth for when network isolation alone is not enough. Only consulted when `AKARI_PROXY_AUTH_HEADER` is set. |
| `AKARI_PROXY_AUTH_SECRET_HEADER` | `X-Akari-Proxy-Secret` | The header carrying `AKARI_PROXY_AUTH_SECRET`. Only consulted when that secret is set. |
| `AKARI_PASSWORD_WORKERS` | `2` | Maximum Argon2 password hashes and verifications running at once. Each worker can use 64 MiB, so size this from the memory available to the server. Must be positive. |
| `AKARI_PASSWORD_QUEUE_DEPTH` | `32` | Maximum password operations waiting behind active workers. Requests beyond this bound fail closed. Must be positive. |
| `AKARI_PASSWORD_QUEUE_TIMEOUT` | `3s` | Maximum time an admitted password operation waits for a worker. A Go duration; must be positive. |
| `AKARI_SWEEP_INTERVAL` | `1h` | How often the server reclaims orphaned content-addressed blobs. A Go duration (`30m`, `2h`); `0` disables the background sweep. |
| `AKARI_OG_CACHE_TTL` | `1h` | How long a rendered Open Graph preview card of a published overview is served from cache before the next request re-renders it. A Go duration; must be positive. |
| `AKARI_OG_CLEANUP_INTERVAL` | `24h` | How often the server prunes expired preview cards (older than `AKARI_OG_CACHE_TTL`) from the cache. A Go duration; `0` disables the sweep. |
| `AKARI_INSIGHTS_REFRESH_INTERVAL` | `1h` | How often the fleet Insights snapshot recomputes in the background. Every trailing window recomputes together in one pass, so the range views cannot drift apart; the page notes the snapshot's age beside its range selector. A Go duration; `0` disables the background loop (the snapshot then computes on first request and when a reparse completes). |
| `AKARI_SIGNALS_SETTLE_INTERVAL` | `5m` | How often the server computes per-session quality signals (outcome, grade, prompt hygiene, context health) for sessions that have settled: a session is graded once it has been idle past the abandoned threshold (30 minutes), off the ingest path, so a live session is never graded with a verdict that would drift. A session an ephemeral host declared terminal (`akari sync --finalize`) is graded immediately instead, both by this pass and by the finalize call the client makes at the end of the sync, so the grade lands before the host is torn down. A Go duration; `0` disables the background pass (signals then land only on reparse, the finalize call, or `akari-server settle`). |
| `AKARI_REQUEST_BUDGET_CAPACITY` | `16` | Process-wide weighted capacity for expensive public work. Must be at least `12`, the weight of one maximum-sized MCP POST under the 100 MiB ceiling tracked by issue #134. Password work weighs `8`, public analytics `4`, MCP POST parsing and spooling `12`, and dynamic OAuth registration `1`. |
| `AKARI_REQUEST_BUDGET_WAIT_TIMEOUT` | `5s` | Maximum time expensive work waits for capacity. A timed-out request receives HTTP 503 with `Retry-After: 1`. Must be a positive Go duration. |
| `AKARI_OAUTH_REGISTRATIONS_PER_HOUR` | `1000` | Abuse ceiling for successful dynamic OAuth client registrations in a rolling hour. Postgres coordinates this limit across all server replicas. Excess registrations receive HTTP 429 with `Retry-After: 3600`. |
### Browser origin and reverse proxies

akari rejects unsafe browser requests unless they come from its public origin.
This applies to login and registration as well as signed-in account, publication,
OAuth consent, and other mutations. `Origin`, when present, must exactly match the
configured origin. `Sec-Fetch-Site`, when present, must be `same-origin`; a
`same-site` sibling is rejected. A malformed or conflicting header is always
rejected.

The built-in forms also send a double-submit token. It is the fallback when a
client or proxy path omits both browser headers. A non-browser client that must
use a session cookie can first load a form page, retain the `akari_csrf` cookie,
then echo its value in `X-Akari-CSRF-Token` on the write. API clients should use
Bearer tokens instead. Bearer-authenticated ingest and MCP requests do not use
the cookie CSRF gate, and neither do the public OAuth registration and token
protocol endpoints.

Set `AKARI_PUBLIC_URL` to the browser-visible origin in production, especially
when TLS terminates at a reverse proxy:

```sh
AKARI_PUBLIC_URL=https://akari.example.com
```

With that setting, akari compares browser requests to the configured value and
does not use the internal upstream address. If it is unset, akari derives the
origin from the request's TLS state or `X-Forwarded-Proto`, plus `Host`. A reverse
proxy using derived mode must overwrite both headers with the values it received
on its public listener. Do not append to client-supplied forwarding headers.

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

Login and registration share the password-work limits above. Unknown usernames
run a dummy Argon2 verification after admission, so an invalid login does not
expose whether the account exists through the ordinary fast path. Process-local
abuse ceilings also bound sustained attempts per normalized username and direct
network peer. Akari does not trust `X-Forwarded-For` for this purpose; deployments
with several replicas should enforce a fleet-wide source limit at the trusted
edge as well.

**Behind a reverse proxy, the per-source limit becomes a single shared bucket.**
Every request the server sees arrives from the proxy's own address, so the
per-source ceiling stops distinguishing one client from another and instead caps
the whole instance's login and registration traffic together. A busy moment can
then 401 legitimate logins with nothing to tell them apart from real credential
attacks. The per-username limiter is unaffected and still protects individual
accounts. If you need per-client source limits behind a proxy, enforce them there
instead: the proxy sees the real client address, akari does not.

## Single sign-on behind a trusted proxy

akari's built-in accounts are local: a username and password per person,
invite-gated after the first admin. To run akari inside an environment that
already has its own identity (as a sidecar to another application, or behind your
organization's gateway), it can instead trust identity asserted by a reverse proxy
in front of it. This is the standard identity-aware-proxy pattern: the proxy
authenticates the user against your identity provider, and akari trusts the
username it forwards.

### How it works

Put an authenticating proxy (oauth2-proxy, Pomerium, or your own gateway) in front
of the server. The proxy signs the user in against your IdP and forwards their
username in a request header. Set `AKARI_PROXY_AUTH_HEADER` to that header's name,
and the server will:

- read the username from that header on every request,
- provision an account the first time it sees a new one (with no password, and not
  an admin), and
- treat the request as that signed-in user at full scope, exactly like a browser
  session.

Accounts created this way are federated: they have no local password, so the
[login form](./accounts-and-sharing.md) refuses them. Their only way in is through
the proxy. Everything else (the feed, projects, publishing, and minting API and
[MCP](./agent-access.md) tokens) behaves the same as for a local account.

Because the proxy authenticates every request, deep-linking a user straight into a
page needs no extra step: a link from your other application to
`https://akari.internal/sessions/123` arrives already authenticated as whoever the
proxy says the user is.

### The trust boundary

Turning this on means akari believes anyone who can set the identity header. That
is safe only when akari is reachable **exclusively** through the proxy that sets
it: a private network, a sidecar sharing a pod, or an ingress that always injects
the header. Never expose a proxy-auth instance directly to a network where a client
could set the header itself. Configure the proxy to overwrite (not append) the
identity header, so a client cannot smuggle its own value through.

For defense in depth, set `AKARI_PROXY_AUTH_SECRET` to a value shared out of band
with the proxy. The proxy must echo it in `AKARI_PROXY_AUTH_SECRET_HEADER` (default
`X-Akari-Proxy-Secret`), or akari ignores the identity header, so a client that
reaches the server directly cannot forge an identity without also knowing the
secret. It hardens the boundary; it does not replace network isolation.

### Bootstrapping the admin

A proxy-provisioned account is never an admin, and once any account exists local
registration is invite-only (which needs an admin to mint the invite). So create
the first admin through local password registration **before** you enable proxy
auth: register in a browser to claim the bootstrap admin (see
[The first account](#the-first-account)), then set `AKARI_PROXY_AUTH_HEADER` and
restart. Enable proxy auth on a truly empty database and the first proxied request
creates an ordinary non-admin account, leaving no admin to mint invites or run a
reparse.

### Example

With oauth2-proxy in front, forwarding the authenticated username to the akari
upstream it protects:

```sh
# oauth2-proxy is configured to pass the signed-in user to its upstream, e.g.
#   --pass-user-headers  (sends X-Forwarded-Preferred-Username / X-Auth-Request-*)
# Tell akari which of those headers carries the username:
AKARI_PROXY_AUTH_HEADER=X-Auth-Request-Preferred-Username
```

Point oauth2-proxy's upstream at the akari server, and make sure only the proxy can
reach akari's `AKARI_LISTEN` port (a private network or a shared pod). The exact
header name and the flag that emits it vary by proxy and version, so match
`AKARI_PROXY_AUTH_HEADER` to whatever your proxy actually sends.

Native OIDC login (akari as a relying party, provisioning users on first login) and
SCIM provisioning are planned, so you will be able to point akari straight at an
identity provider and manage the account lifecycle from it. Until then, the
reverse-proxy pattern above is the supported integration.

## Serving under a path prefix

akari does not have to own its origin: a reverse proxy can mount it under any
path, so `https://ops.example.com/tools/akari/` works beside whatever else the
host serves. Tell akari the prefix one of two ways:

- **Statically**, by putting the path in `AKARI_PUBLIC_URL`:

  ```sh
  AKARI_PUBLIC_URL=https://ops.example.com/tools/akari
  ```

- **Per request**, by setting `AKARI_PREFIX_HEADER` to a header your proxy sets
  to the mount path (most proxies call it `X-Forwarded-Prefix`):

  ```sh
  AKARI_PREFIX_HEADER=X-Forwarded-Prefix
  ```

  The header form needs no restart to move the mount and lets one instance be
  reached under a prefix through the proxy and at the root directly (a request
  without the header resolves no prefix). It carries the same trust rule as
  proxy single sign-on: only enable it when akari is reachable exclusively
  through the proxy, and if `AKARI_PROXY_AUTH_SECRET` is set the prefix header
  counts only on requests that carry the matching secret. Set the header on
  every request forwarded under the mount: cookie `Path` scopes follow the
  resolved prefix, so a mount whose requests carry the header inconsistently
  would mint cookies that other requests never present.

With a prefix resolved, akari accepts the forwarded path stripped or
unstripped (proxies differ; both route the same), generates every URL under
the prefix (redirects, asset and API paths in served pages, Open Graph tags,
the OAuth discovery documents, `llms.txt`), and scopes its session and CSRF
cookies to the prefix so sibling applications on the same origin never receive
them.

A Caddy mount looks like:

```
ops.example.com {
	handle /tools/akari/* {
		reverse_proxy localhost:8080 {
			header_up X-Forwarded-Prefix /tools/akari
		}
	}
}
```

Two paths live outside the prefix and deserve a thought:

- **MCP OAuth discovery.** RFC 8414 and RFC 9728 put well-known documents at
  the origin root with the mount path appended, so an MCP client connecting to
  `https://ops.example.com/tools/akari/mcp` fetches
  `/.well-known/oauth-authorization-server/tools/akari`. akari answers those
  suffixed paths; the proxy must forward `/.well-known/oauth-*` to akari for
  agent [OAuth connections](./agent-access.md) to work. The suffix itself
  names the mount, so this forwarding rule does not need to set the prefix
  header. Tokens pasted manually need nothing extra.
- **`/favicon.ico`.** Browsers probe the origin root for it unprompted. Pages
  link the icon under the prefix, so tabs render it either way; the root probe
  belongs to whatever owns the root of that origin.

Ingest and MCP clients need no special handling: point them at the prefixed
base URL (`akari` client `server = https://ops.example.com/tools/akari`) and
they join their API paths under it.

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
akari-server version          # print the build version and exit
```

`sweep` is the manual form of the periodic blob reclaim; it is safe to run any
time, since blob liveness is computed rather than reference-counted. `settle` is
the manual form of the periodic signals pass: it grades every settled session
missing a current-version signals row, then exits.

`dev-seed` is a development convenience: it creates a few demo accounts (sign in as
`grace`, the admin, with password `akari-dev`) and ingests this machine's real
agent sessions. It is idempotent (a no-op once the store holds sessions) and
best-effort by default. Keep it away from
any server holding real data.

## Upgrading

The server has no self-update command. The deployment mechanism that owns the
server process also owns upgrades. Pin the replacement to a release tag so the
running code and its reported version are reproducible.

### Container deployment

Pull the versioned image from GitHub Container Registry and redeploy it through
the same container orchestrator that runs the current image. Release images
support Linux amd64 and arm64:

```sh
docker pull ghcr.io/jssblck/akari-server:v0.1.0
docker run --rm ghcr.io/jssblck/akari-server:v0.1.0 --version
docker image inspect --format '{{index .RepoDigests 0}}' \
  ghcr.io/jssblck/akari-server:v0.1.0
```

Update the deployment to that versioned image. Stable releases also update
`latest`, but production deployments should use `vX.Y.Z` or, when the deployed
bytes must remain fixed across release-workflow reruns, the digest printed
above. Do not replace a binary inside a running container; the next container
restart would restore the old image contents.

### Package or managed binary

When a package manager owns the server, install the selected package version and
restart the service through that package's normal supervisor integration.

For the systemd installation created by `install-server.sh`, run the installer
from the release tag you intend to deploy and pass that same tag as
`AKARI_VERSION`, then restart the service:

```sh
curl -fsSL https://raw.githubusercontent.com/jssblck/akari/v0.1.0/scripts/install-server.sh \
  | sudo AKARI_VERSION=v0.1.0 AKARI_INSTALL_DIR=/usr/local/bin sh
sudo systemctl restart akari-server
akari-server version
```

The installer verifies the release archive against its published checksum
before replacing the binary. Existing `/etc/akari/server.env` configuration is
unchanged, and the replacement server applies embedded database migrations when
it starts. The same sequence works with another service supervisor: install a
specific checksum-verified release archive, restart the managed process, and
verify its reported version.

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
- **Scrape `/metrics`** for request-budget queue depth, wait time, rejection
  counts, and per-class utilization. The Prometheus text response contains no
  user or request identifiers. The endpoint is unauthenticated by design (a
  scraper needs no credential), so it should not be publicly reachable;
  restrict it to your metrics collector at the reverse proxy.
- **If you use reverse-proxy single sign-on**, make sure the server is reachable
  only through the proxy that sets the identity header (see
  [Single sign-on behind a trusted proxy](#single-sign-on-behind-a-trusted-proxy)).

The server shuts down gracefully on interrupt: it drains in-flight requests and
lets background work (sweep, card refresh, any reparse) wind down before the
connection pool closes.

The weighted request budget is process-local. Each replica protects its own CPU,
memory, temporary disk, and database concurrency, so aggregate admission capacity
scales with the replica count. The dynamic OAuth registration ceiling is
serialized in Postgres and remains deployment-wide.

---

Next: [Glossary](./glossary.md) -> the terms the guide uses, for reference.
