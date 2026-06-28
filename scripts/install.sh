#!/bin/sh
# Install the akari client on Linux or macOS.
#
# Downloads the release archive that matches this machine's OS and architecture
# from GitHub Releases, verifies it against the release SHA256SUMS, and installs
# the `akari` binary into a bin directory on your PATH.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/jssblck/akari/main/scripts/install.sh | sh
#
# Environment overrides:
#   AKARI_VERSION       version tag to install (default: the latest release), e.g. v0.1.0
#   AKARI_INSTALL_DIR   directory to install into (default: $HOME/.local/bin)
#   AKARI_REPO          owner/repo to download from (default: jssblck/akari)
#   AKARI_BASE_URL      base URL holding the archive and SHA256SUMS (default: the
#                       GitHub release download URL for the resolved tag)

set -eu

REPO="${AKARI_REPO:-jssblck/akari}"
BIN="akari"

info() { printf 'akari install: %s\n' "$1" >&2; }
err() { printf 'akari install: error: %s\n' "$1" >&2; exit 1; }

# need verifies a required command is available, with a hint when it is not.
need() {
	command -v "$1" >/dev/null 2>&1 || err "required command not found: $1"
}

# A downloader is required; prefer curl, fall back to wget. fetch writes the URL
# to stdout; download writes it to a file.
if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL "$1"; }
	download() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -qO- "$1"; }
	download() { wget -qO "$2" "$1"; }
else
	err "need curl or wget to download the release"
fi
need tar
need uname

# sha256 verification uses whichever tool the platform ships (sha256sum on
# Linux, shasum on macOS). sha256_of prints the lowercase hex digest of a file.
if command -v sha256sum >/dev/null 2>&1; then
	sha256_of() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256_of() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	err "need sha256sum or shasum to verify the download"
fi

# Map uname output to the Go GOOS/GOARCH names used in the release filenames.
os=$(uname -s)
case "$os" in
	Linux) os="linux" ;;
	Darwin) os="darwin" ;;
	*) err "unsupported OS: $os (the client ships for linux and darwin; on Windows use install.ps1)" ;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64 | amd64) arch="amd64" ;;
	aarch64 | arm64) arch="arm64" ;;
	*) err "unsupported architecture: $arch" ;;
esac

# Resolve the tag to install: an explicit AKARI_VERSION, otherwise the latest
# published (non-prerelease) release from the GitHub API. The asset filename
# embeds the version without the leading v, matching the release workflow.
tag="${AKARI_VERSION:-}"
if [ -z "$tag" ]; then
	info "resolving the latest release of $REPO"
	tag=$(fetch "https://api.github.com/repos/$REPO/releases/latest" |
		grep '"tag_name"' | head -1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')
	[ -n "$tag" ] || err "could not determine the latest release tag (set AKARI_VERSION to install a specific version)"
fi
version="${tag#v}"

archive="${BIN}_${version}_${os}_${arch}.tar.gz"
base="${AKARI_BASE_URL:-https://github.com/$REPO/releases/download/$tag}"

tmp=$(mktemp -d 2>/dev/null || mktemp -d -t akari)
trap 'rm -rf "$tmp"' EXIT

info "downloading $archive ($tag)"
download "$base/$archive" "$tmp/$archive" || err "failed to download $base/$archive"
download "$base/SHA256SUMS" "$tmp/SHA256SUMS" || err "failed to download SHA256SUMS"

# Verify the archive against the published checksum before extracting anything.
want=$(grep " $archive\$" "$tmp/SHA256SUMS" | cut -d' ' -f1 || true)
[ -n "$want" ] || err "no checksum for $archive in SHA256SUMS"
got=$(sha256_of "$tmp/$archive")
[ "$want" = "$got" ] || err "checksum mismatch for $archive (want $want, got $got)"
info "checksum verified"

tar -xzf "$tmp/$archive" -C "$tmp" "$BIN"
[ -f "$tmp/$BIN" ] || err "archive did not contain $BIN"
chmod +x "$tmp/$BIN"

# Install into a user-writable bin dir by default so no root is needed. Fall
# back to sudo only when the chosen dir exists but is not writable.
dir="${AKARI_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$dir" 2>/dev/null || true
if [ -w "$dir" ] || { [ ! -e "$dir" ] && mkdir -p "$dir"; }; then
	mv "$tmp/$BIN" "$dir/$BIN"
elif command -v sudo >/dev/null 2>&1; then
	info "installing to $dir with sudo"
	sudo mkdir -p "$dir"
	sudo mv "$tmp/$BIN" "$dir/$BIN"
else
	err "cannot write to $dir and sudo is unavailable (set AKARI_INSTALL_DIR to a writable directory)"
fi

info "installed $BIN $tag to $dir/$BIN"

# Nudge the user if the install dir is not on PATH, since the binary would not
# be found otherwise.
case ":$PATH:" in
	*":$dir:"*) ;;
	*) info "note: $dir is not on your PATH; add it, e.g. export PATH=\"$dir:\$PATH\"" ;;
esac

printf '\nNext: point the client at your server and start syncing.\n' >&2
printf '  %s login --server https://akari.example.com --token <ingest-token>\n' "$BIN" >&2
printf '  %s sync\n' "$BIN" >&2
