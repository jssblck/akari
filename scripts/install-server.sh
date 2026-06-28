#!/bin/sh
# Install the akari server on Linux.
#
# Downloads the akari-server release archive for this machine's architecture
# from GitHub Releases, verifies it against the release SHA256SUMS, and installs
# the binary into /usr/local/bin. With --systemd it also installs a systemd
# service, an environment file, and a dedicated system user so the server runs
# as a managed daemon.
#
# The server needs a reachable Postgres; this script does not provision one. For
# an all-in-one local stack (Postgres plus server) use docker-compose instead.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/jssblck/akari/main/scripts/install-server.sh | sh
#   curl -fsSL .../install-server.sh | sh -s -- --systemd
#
# Environment overrides:
#   AKARI_VERSION       version tag to install (default: the latest release), e.g. v0.1.0
#   AKARI_INSTALL_DIR   directory to install into (default: /usr/local/bin)
#   AKARI_REPO          owner/repo to download from (default: jssblck/akari)
#   AKARI_BASE_URL      base URL holding the archive and SHA256SUMS (default: the
#                       GitHub release download URL for the resolved tag)

set -eu

REPO="${AKARI_REPO:-jssblck/akari}"
BIN="akari-server"
SYSTEMD=0

for arg in "$@"; do
	case "$arg" in
		--systemd) SYSTEMD=1 ;;
		-h | --help)
			sed -n '2,30p' "$0" 2>/dev/null || true
			exit 0
			;;
		*) printf 'akari install: error: unknown argument: %s\n' "$arg" >&2; exit 1 ;;
	esac
done

info() { printf 'akari install: %s\n' "$1" >&2; }
err() { printf 'akari install: error: %s\n' "$1" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || err "required command not found: $1"; }

# run executes a command as root: directly when already root, otherwise via sudo.
if [ "$(id -u)" -eq 0 ]; then
	run() { "$@"; }
else
	command -v sudo >/dev/null 2>&1 || err "this script needs root to install to system paths; run as root or install sudo"
	run() { sudo "$@"; }
fi

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

if command -v sha256sum >/dev/null 2>&1; then
	sha256_of() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256_of() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	err "need sha256sum or shasum to verify the download"
fi

# The server ships for Linux only.
[ "$(uname -s)" = "Linux" ] || err "the akari server ships for Linux only (got $(uname -s))"
os="linux"

arch=$(uname -m)
case "$arch" in
	x86_64 | amd64) arch="amd64" ;;
	aarch64 | arm64) arch="arm64" ;;
	*) err "unsupported architecture: $arch" ;;
esac

# Resolve the tag: an explicit AKARI_VERSION, otherwise the latest published
# release from the GitHub API.
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

want=$(grep " $archive\$" "$tmp/SHA256SUMS" | cut -d' ' -f1 || true)
[ -n "$want" ] || err "no checksum for $archive in SHA256SUMS"
got=$(sha256_of "$tmp/$archive")
[ "$want" = "$got" ] || err "checksum mismatch for $archive (want $want, got $got)"
info "checksum verified"

tar -xzf "$tmp/$archive" -C "$tmp" "$BIN"
[ -f "$tmp/$BIN" ] || err "archive did not contain $BIN"
chmod +x "$tmp/$BIN"

dir="${AKARI_INSTALL_DIR:-/usr/local/bin}"
run mkdir -p "$dir"
run install -m 0755 "$tmp/$BIN" "$dir/$BIN"
info "installed $BIN $tag to $dir/$BIN"

if [ "$SYSTEMD" -eq 0 ]; then
	printf '\nRun the server with a Postgres URL in the environment:\n' >&2
	printf '  AKARI_DATABASE_URL=postgres://... %s/%s\n' "$dir" "$BIN" >&2
	printf '\nRe-run with --systemd to install a managed systemd service.\n' >&2
	exit 0
fi

# --systemd: set up a managed daemon. A dedicated unprivileged user owns the
# process; the config lives in an environment file the unit reads. The service
# is installed but not started, because it cannot run until AKARI_DATABASE_URL
# points at a real database.
need systemctl

if ! id akari >/dev/null 2>&1; then
	info "creating system user 'akari'"
	run useradd --system --no-create-home --shell /usr/sbin/nologin akari
fi

run mkdir -p /etc/akari
env_file=/etc/akari/server.env
if [ ! -e "$env_file" ]; then
	info "writing $env_file (edit it to set AKARI_DATABASE_URL)"
	run sh -c "cat > '$env_file'" <<'ENV'
# akari server configuration.
# See https://github.com/jssblck/akari#server-configuration for all options.

# Required: Postgres connection string.
AKARI_DATABASE_URL=postgres://akari:akari@localhost:5432/akari?sslmode=disable

# Address the HTTP server binds.
AKARI_LISTEN=:8080

# How often to reclaim orphaned content-addressed blobs (Go duration; 0 disables).
AKARI_SWEEP_INTERVAL=1h
ENV
	run chgrp akari "$env_file"
	run chmod 0640 "$env_file"
else
	info "$env_file already exists; leaving it unchanged"
fi

unit=/etc/systemd/system/akari-server.service
info "writing $unit"
run sh -c "cat > '$unit'" <<UNIT
[Unit]
Description=akari server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=akari
Group=akari
EnvironmentFile=$env_file
ExecStart=$dir/$BIN
Restart=on-failure
RestartSec=5
# Hardening: the server needs no elevated privileges or write access outside
# its runtime dir, so lock the rest of the filesystem down.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
UNIT

run systemctl daemon-reload
info "installed the akari-server systemd service"
printf '\nNext steps:\n' >&2
printf '  1. Edit %s and set AKARI_DATABASE_URL to your Postgres.\n' "$env_file" >&2
printf '  2. sudo systemctl enable --now akari-server\n' >&2
printf '  3. sudo systemctl status akari-server\n' >&2
