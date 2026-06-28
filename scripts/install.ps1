<#
.SYNOPSIS
Install the akari client on Windows.

.DESCRIPTION
Downloads the release archive that matches this machine's architecture from
GitHub Releases, verifies it against the release SHA256SUMS, and installs
akari.exe into a per-user bin directory, adding that directory to the user PATH
when it is not already there.

Usage:
  irm https://raw.githubusercontent.com/jssblck/akari/main/scripts/install.ps1 | iex

Environment overrides:
  AKARI_VERSION       version tag to install (default: the latest release), e.g. v0.1.0
  AKARI_INSTALL_DIR   directory to install into (default: %LOCALAPPDATA%\akari\bin)
  AKARI_REPO          owner/repo to download from (default: jssblck/akari)
  AKARI_BASE_URL      base URL holding the archive and SHA256SUMS (default: the
                      GitHub release download URL for the resolved tag)
#>

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$repo = if ($env:AKARI_REPO) { $env:AKARI_REPO } else { "jssblck/akari" }
$bin = "akari"

function Info($msg) { Write-Host "akari install: $msg" }
function Die($msg) { Write-Error "akari install: $msg"; exit 1 }

# The client ships windows/amd64 and windows/arm64. Map the process architecture
# to the Go GOARCH name used in the release filenames.
$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    "AMD64" { "amd64" }
    "ARM64" { "arm64" }
    "x86"   { Die "32-bit x86 is not supported" }
    default { Die "unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
}

# Resolve the tag: an explicit AKARI_VERSION, otherwise the latest published
# release from the GitHub API.
$tag = $env:AKARI_VERSION
if (-not $tag) {
    Info "resolving the latest release of $repo"
    $latest = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest" -Headers @{ "User-Agent" = "akari-install" }
    $tag = $latest.tag_name
    if (-not $tag) { Die "could not determine the latest release tag (set AKARI_VERSION to install a specific version)" }
}
$version = $tag -replace '^v', ''

$archive = "${bin}_${version}_windows_${arch}.zip"
$base = if ($env:AKARI_BASE_URL) { $env:AKARI_BASE_URL } else { "https://github.com/$repo/releases/download/$tag" }

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("akari-install-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    $archivePath = Join-Path $tmp $archive
    $sumsPath = Join-Path $tmp "SHA256SUMS"

    Info "downloading $archive ($tag)"
    Invoke-WebRequest -Uri "$base/$archive" -OutFile $archivePath -Headers @{ "User-Agent" = "akari-install" }
    Invoke-WebRequest -Uri "$base/SHA256SUMS" -OutFile $sumsPath -Headers @{ "User-Agent" = "akari-install" }

    # Verify the archive against the published checksum before extracting.
    $want = (Get-Content $sumsPath | Where-Object { $_ -match "\s$([regex]::Escape($archive))$" } |
        Select-Object -First 1) -replace '\s.*$', ''
    if (-not $want) { Die "no checksum for $archive in SHA256SUMS" }
    $got = (Get-FileHash -Algorithm SHA256 -LiteralPath $archivePath).Hash.ToLowerInvariant()
    if ($want.ToLowerInvariant() -ne $got) { Die "checksum mismatch for $archive (want $want, got $got)" }
    Info "checksum verified"

    Expand-Archive -LiteralPath $archivePath -DestinationPath $tmp -Force
    $exe = Join-Path $tmp "$bin.exe"
    if (-not (Test-Path $exe)) { Die "archive did not contain $bin.exe" }

    $dir = if ($env:AKARI_INSTALL_DIR) { $env:AKARI_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "akari\bin" }
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    Copy-Item -LiteralPath $exe -Destination (Join-Path $dir "$bin.exe") -Force
    Info "installed $bin $tag to $dir\$bin.exe"

    # Add the install dir to the user PATH (persisted) and the current session if
    # it is not already present, so `akari` resolves in new and existing shells.
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (($userPath -split ';') -notcontains $dir) {
        [Environment]::SetEnvironmentVariable("Path", ($userPath.TrimEnd(';') + ";$dir"), "User")
        Info "added $dir to your user PATH (restart your shell to pick it up)"
    }
    if (($env:Path -split ';') -notcontains $dir) { $env:Path = "$env:Path;$dir" }
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

Write-Host ""
Write-Host "Next: point the client at your server and start syncing."
Write-Host "  $bin login --server https://akari.example.com --token <ingest-token>"
Write-Host "  $bin sync"
