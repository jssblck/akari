# akari

akari collects the local session logs of coding agents (Claude Code, Codex, and
pi) on a self-hosted server: a searchable history of every session across your
machines, grouped by the git project they ran in, with every token priced. It
is an explicit client/server split. Thin clients push raw session bytes; the
server parses, prices, and serves a web UI and a read-only MCP endpoint. Because
the client keeps no derived state, a parser improvement reaches old sessions by
re-parsing on the server, with nothing re-uploaded.

## Documentation

The full user guide lives on the running server itself, themed to match the UI:
open [`/guide`](https://akari.jessica.black/guide) on the main instance, and
every akari server serves its own copy at `/guide`. The guide is written for
agents as much as humans: append `.md` to any page URL for its raw Markdown,
fetch [`/llms-full.txt`](https://akari.jessica.black/llms-full.txt) for the whole
guide in one request, and [`/llms.txt`](https://akari.jessica.black/llms.txt) for
the machine-readable index. The source lives in
[`internal/guide/content`](internal/guide/content).

The browser-facing JSON API is documented by the server at `/api/docs`; its
OpenAPI 3.1 contract is available directly at `/api/openapi.json`.

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

Server OCI image, Linux amd64 and arm64:

```sh
docker pull ghcr.io/jssblck/akari-server:v0.1.0
```

Add `-s -- --systemd` to the server command to also install a managed systemd
service, a dedicated `akari` user, and an environment file at
`/etc/akari/server.env`. The client updates itself in place with `akari update`.
Upgrade the server by deploying a versioned container image or package, or by
replacing the managed binary and restarting its service supervisor. See
[docs/releases.md](docs/releases.md) for the asset list and upgrade options.

## Quick start

After installing, mint an ingest token on the server's account page, then point
the client at your server and start pushing:

```sh
akari login --server https://akari.example.com --token <ingest-token>
akari sync            # one-shot upload of everything new
akari daemon start    # keep uploading in the background
```

The [getting-started chapter](https://akari.jessica.black/guide/getting-started)
of the guide covers the rest.

No server yet? `docker compose up -d --build` with the bundled compose file
stands one up; the
[self-hosting chapter](https://akari.jessica.black/guide/self-hosting) covers
real deployments.

## Development

Orientation for agents and contributors is [AGENTS.md](AGENTS.md). The
development loop (build, tests, eph, seed data) is
[docs/development.md](docs/development.md), the engineering design is
[docs/DESIGN.md](docs/DESIGN.md), and the visual design system is
[DESIGN.md](DESIGN.md). Contributions follow
[CONTRIBUTING.md](CONTRIBUTING.md).

## License

akari follows the repository license split described in [`NOTICE`](NOTICE):
runtime software is AGPL-3.0-or-later, while documentation and creative content
are CC-BY-SA-4.0 unless a file says otherwise. See also the
[security policy](SECURITY.md), the [contributing guide](CONTRIBUTING.md), and
the [code of conduct](CODE_OF_CONDUCT.md).
