// Package version reports the build version of the akari binaries.
//
// Both akari and akari-server print this through their `version` command (and
// the `--version` flag). The value is resolved with the same precedence the
// release pipeline relies on:
//
//  1. An explicit ldflags override. Release CI builds with
//     -ldflags "-X github.com/jssblck/akari/internal/version.Version=v1.2.3"
//     so the reported version is exactly the release tag, regardless of how the
//     repository was cloned.
//  2. Otherwise the VCS revision the Go toolchain embeds in the binary
//     (debug.ReadBuildInfo), so a plain `go build` still reports the commit it
//     was built from with a "-dirty" suffix for uncommitted changes.
//  3. Otherwise "dev", for a build with no version override and no embedded VCS
//     stamp (for example `go run` against a source tree with no .git).
package version

import "runtime/debug"

// Version is the build version. It is "dev" by default and overridden at link
// time by release CI via -ldflags "-X .../internal/version.Version=<tag>". Read
// it through String, which applies the VCS-revision fallback.
var Version = "dev"

// String returns the version to report to users. It prefers an ldflags override
// and falls back to the embedded VCS revision so an untagged local build still
// reports something meaningful instead of a bare "dev".
func String() string {
	if Version != "dev" {
		return Version
	}
	if v := vcsVersion(); v != "" {
		return v
	}
	return Version
}

// vcsVersion derives a short version string from the build info the Go
// toolchain embeds. It returns the short commit revision with a "-dirty" suffix
// when the working tree was modified, or "" when no VCS stamp is available (a
// build from a source tarball, or a build run with -buildvcs=false).
func vcsVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision == "" {
		return ""
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified == "true" {
		return revision + "-dirty"
	}
	return revision
}
