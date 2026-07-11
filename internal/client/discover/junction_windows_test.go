//go:build windows

package discover

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// mkjunction creates a Windows directory junction at link pointing at target,
// via `mklink /J`, the same privilege-free mechanism a user relocating their
// agent session directory would use (unlike a true directory symlink, a
// junction needs no elevated privilege or Developer Mode, which is why this
// package's regression coverage for the reparse-point root bug uses it rather
// than os.Symlink).
func mkjunction(t *testing.T, link, target string) {
	t.Helper()
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf("mklink /J %s -> %s: %v: %s", link, target, err, out)
	}
}

// TestDiscoverRejectsJunctionRootByDefault covers the reported blocker: a
// directory junction root (an extra_root or an agent env-var override root, both
// modeled here as a non-Optional Root) must be rejected with a clear error
// naming it a directory junction, not misreported as "root is not a directory"
// (Go's Lstat reports a junction as ModeIrregular, neither ModeSymlink nor a
// directory) and not silently walked.
func TestDiscoverRejectsJunctionRootByDefault(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	write(t, filepath.Join(target, "session.jsonl"))
	link := filepath.Join(dir, "link")
	mkjunction(t, link, target)

	files, notices, err := Discover([]Root{{Agent: "claude", Dir: link}}, Excluder{})
	if len(files) != 0 {
		t.Fatalf("junction root was walked: %+v", files)
	}
	if len(notices) != 0 {
		t.Fatalf("non-Optional junction root produced a notice instead of an error: %v", notices)
	}
	if ErrorCount(err) != 1 {
		t.Fatalf("ErrorCount = %d, want 1: %v", ErrorCount(err), err)
	}
	if !strings.Contains(err.Error(), "directory junction") {
		t.Fatalf("error %q does not name the junction", err)
	}
}

// TestDiscoverSkipsOptionalJunctionRootWithNotice covers the built-in-root
// exception: a linked Optional root (the shape of the claude/codex/pi standard
// directories) is skipped quietly rather than failing discovery, since a user
// who junctioned their agent directory should not see `akari sync` start
// exiting nonzero over it. It is reported via a notice, not counted as a
// discovery error.
func TestDiscoverSkipsOptionalJunctionRootWithNotice(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	write(t, filepath.Join(target, "session.jsonl"))
	link := filepath.Join(dir, "link")
	mkjunction(t, link, target)

	files, notices, err := Discover([]Root{{Agent: "claude", Dir: link, Optional: true}}, Excluder{})
	if len(files) != 0 {
		t.Fatalf("junction root was walked: %+v", files)
	}
	if err != nil {
		t.Fatalf("Optional junction root should not error: %v", err)
	}
	if ErrorCount(err) != 0 {
		t.Fatalf("ErrorCount = %d, want 0", ErrorCount(err))
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "directory junction") || !strings.Contains(notices[0], link) {
		t.Fatalf("notices = %v, want one naming the junction at %s", notices, link)
	}
}

// TestDiscoverFollowsJunctionRootWhenOptedIn covers the explicit opt-in from
// issue #140: with FollowRootLink set on the root, discovery resolves the
// junction to its target and walks it, finding the files there. The no-follow
// policy still applies inside the walk (unexercised here, covered by
// TestDiscoverDoesNotFollowDirectorySymlinkLoop and friends), only the root link
// itself is followed.
func TestDiscoverFollowsJunctionRootWhenOptedIn(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	write(t, filepath.Join(target, "nested", "session.jsonl"))
	link := filepath.Join(dir, "link")
	mkjunction(t, link, target)

	files, notices, err := Discover([]Root{{Agent: "claude", Dir: link, FollowRootLink: true}}, Excluder{})
	if err != nil {
		t.Fatalf("opted-in junction root should not error: %v", err)
	}
	if len(notices) != 0 {
		t.Fatalf("opted-in junction root should not produce a notice: %v", notices)
	}
	if len(files) != 1 || files[0].Path != filepath.Join(target, "nested", "session.jsonl") {
		t.Fatalf("files = %+v, want the one session found through the followed junction", files)
	}
	// The file's recorded discovery root is the resolved target, not the link, so
	// resolution's relative-id math lines up with the paths WalkDir actually
	// reported.
	if files[0].Root != target {
		t.Fatalf("file root = %q, want resolved target %q", files[0].Root, target)
	}
}
