# Releases

This page explains how akari is versioned, how to cut a release, and what each
published asset is.

## Versioning

Versions come from git tags shaped like `vX.Y.Z` (for example `v0.1.0`).

The version a binary reports through `akari version` / `akari-server version`
(and the `--version` flag) is resolved at build time in
[`internal/version`](../internal/version/version.go) with this precedence:

1. An explicit `-ldflags -X` override. Release CI builds with
   `-ldflags "-X github.com/jssblck/akari/internal/version.Version=<tag>"` so the
   reported version is exactly the release tag, regardless of clone depth.
2. Otherwise the VCS revision the Go toolchain embeds (`debug.ReadBuildInfo`), so
   a plain `go build` reports the commit it was built from, with a `-dirty`
   suffix for uncommitted changes.
3. Otherwise `dev`, when there is neither an override nor an embedded VCS stamp.

There is no version string to bump in source; the tag is the source of truth.
Do not edit a constant to mark a release, tag instead.

## Cutting a release

1. Make sure `main` is in the state you want to ship.
2. Create and push a version tag:

   ```sh
   git tag v0.1.0
   git push origin v0.1.0
   ```

3. The `Release` workflow cross-compiles every target, packages the archives,
   computes `SHA256SUMS`, then creates a DRAFT GitHub Release with notes
   generated from the pull requests merged since the previous tag.
4. Review the draft release, edit the generated notes if needed, then publish it
   by hand. A bare `X.Y.Z` tag is published as the latest stable release; any
   other tag shape (for example a `-rc.1` pre-release suffix) is marked as a
   prerelease.

## Published assets

Every release attaches one archive per build target plus a checksum file. The
archives are named `<bin>_<version>_<os>_<arch>` (the version without the leading
`v`, matching the goreleaser convention) and each contains the binary and a copy
of `README.md`.

- **Server** (`akari-server`), Linux only, since the server is deployed as a
  container or a Linux host daemon:
  - `akari-server_<version>_linux_amd64.tar.gz`
  - `akari-server_<version>_linux_arm64.tar.gz`
- **Client** (`akari`), for Linux, macOS, and Windows, each on amd64 and arm64:
  - `akari_<version>_linux_amd64.tar.gz`
  - `akari_<version>_linux_arm64.tar.gz`
  - `akari_<version>_darwin_amd64.tar.gz`
  - `akari_<version>_darwin_arm64.tar.gz`
  - `akari_<version>_windows_amd64.zip`
  - `akari_<version>_windows_arm64.zip`
- `SHA256SUMS`: a sha256 plus filename line for every archive, for manual
  verification (`sha256sum -c SHA256SUMS`).

All targets cross-compile from a single Linux runner with `CGO_ENABLED=0`: akari
is pure Go, so there is no per-OS runner matrix. The binaries are built with
`-trimpath -ldflags "-s -w -X .../version.Version=<tag>"`, so they are stripped,
reproducible, and report the tag through `--version`.

## Container image

The `Dockerfile` builds `akari-server`. To stamp the version into the container,
pass the tag as a build arg:

```sh
docker build --build-arg VERSION=v0.1.0 -t akari-server:v0.1.0 .
```

Without the arg the image reports `dev` (the `.git` directory is excluded from
the Docker build context, so there is no VCS stamp to fall back to). The release
workflow does not publish a container image; only the binary archives above are
attached to the GitHub Release.

## Dry run

The `Release` workflow runs as a dry run whenever it is not triggered by a
version tag:

- On every pull request (so a change that breaks the release build fails on the
  PR, not when a tag is cut).
- On every push to `main`.
- On a manual `workflow_dispatch` from the Actions tab.

A dry run builds and packages every archive and computes `SHA256SUMS`, then
writes a job summary, but it does not create a GitHub Release. The only thing
that turns a dry run into a real release is the trigger being a `vX.Y.Z` tag. On
a non-tag ref the version is derived from `git describe` and treated as a
prerelease.
