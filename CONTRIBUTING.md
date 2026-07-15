# Contributing

akari is an application, not a Go library intended for import. Contributions
should improve the actual product: the client's discovery and upload path, the
per-agent parsers, the server's storage and parse pipeline, the web UI, the
pricing table, or the maintainability of the system.

## Local setup

Install Go (the version in `go.mod`), Bun, and Git. The application UI is React
under `frontend/`; its committed production build is embedded in the Go binary.
The root homepage uses [templ](https://templ.guide), pinned as a Go tool in
`go.mod`, and its generated `internal/server/web/*_templ.go` files are
gitignored. Use the Makefile to keep both frontend layers and Go in step:

```sh
make build        # build React, generate templ, then build Go
make test         # check, test, and build React, then run Go tests under -race
make vet
make fmt          # report files that are not gofmt-clean
```

Integration tests that touch Postgres skip cleanly unless
`AKARI_TEST_DATABASE_URL` is set. Under [eph](docs/development.md#worktree-based-development-with-eph)
the variable is already set, so `eph run go test ./...` covers the store, parse,
and web tests too. Without eph, point it at any Postgres whose role may create
databases; each test provisions and drops its own database. See
[docs/development.md](docs/development.md) for the full loop.

## Working style

- Fix root causes when they are in scope.
- Do not preserve backwards compatibility by default; if the clean solution means
  changing schemas, renaming concepts, or rewriting call sites, do it and mention
  the breakage plainly.
- React under `frontend/` owns the application routes. Run `make frontend` after
  changing it and commit the rebuilt `internal/server/frontend/dist/` artifact.
  The `.templ` files own only the root homepage; regenerate them after edits and
  never commit the generated `*_templ.go` files.
- Keep a schema change and its migration together: `migrations` holds the
  embedded SQL, and the server reparses stored sessions in the background when the
  parser changes, so a parser or projection change should still round-trip old
  data.
- Keep the docs under `docs/` (and the README) in sync when behavior changes.
- Run `make test` before opening a PR.
- Use plain ASCII quotes in docs, comments, and generated text.

## AI-assisted contributions

AI-assisted PRs are welcome. The human submitter is responsible for the change:
understand the code, review the generated output, test it, and explain the
intent. Do not submit a raw dump of generated code that you cannot defend or
maintain. Maintainers may ask for simplification, tests, or clearer rationale
before merging.

## Pull requests

PRs should explain why the change exists, what behavior changed, any impact on
the stored schema or the parse projection, and the verification performed
(especially the Go checks and any database-backed tests).

## Releases

akari ships binaries on GitHub Releases, not a Go module for import, so a release
is just a tag: there is no package publish and no version file to bump. GitHub
Releases double as the changelog, so there is no hand-maintained changelog file
either. The reported version is stamped into the binary from the git tag.

To cut a release:

1. Make sure `main` is green and points at the commit you want to ship.
2. Tag it in the shape `vX.Y.Z` and push the tag.

   ```sh
   git tag v0.2.0
   git push origin v0.2.0
   ```

3. The [release workflow](.github/workflows/release.yml) cross-compiles the
   server (Linux) and the client (Linux, macOS, Windows), packages each target
   into an archive, computes a `SHA256SUMS`, and opens a **draft** GitHub Release
   whose notes are generated from the pull requests merged since the previous tag.
4. Review the draft and its generated notes, edit if needed, and publish.

The same build runs as a dry run on every pull request and `main` push, so a
break in the release pipeline surfaces on the PR rather than when a tag is cut. A
tag with a pre-release suffix (`v0.2.0-rc.1`) is published as a prerelease. See
[docs/releases.md](docs/releases.md) for the full process and asset list.
