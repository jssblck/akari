//go:build unix

package discover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiscoverReportsInaccessibleRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can traverse permission fixtures")
	}
	root := filepath.Join(t.TempDir(), "blocked")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

	files, err := Discover([]Root{{Agent: "claude", Dir: root}}, Excluder{})
	if len(files) != 0 {
		t.Fatalf("discovered files in inaccessible root: %+v", files)
	}
	if ErrorCount(err) != 1 || !strings.Contains(err.Error(), root) {
		t.Fatalf("inaccessible root error = %v", err)
	}
}

func TestDiscoverReturnsPartialFilesWithMidWalkError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can traverse permission fixtures")
	}
	root := t.TempDir()
	safe := filepath.Join(root, "a-safe.jsonl")
	write(t, safe)
	blocked := filepath.Join(root, "z-blocked")
	write(t, filepath.Join(blocked, "hidden.jsonl"))
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o700) })

	files, err := Discover([]Root{{Agent: "claude", Dir: root}}, Excluder{})
	if len(files) != 1 || files[0].Path != safe {
		t.Fatalf("partial files = %+v, want safe file", files)
	}
	if ErrorCount(err) != 1 || !strings.Contains(err.Error(), blocked) {
		t.Fatalf("mid-walk error = %v", err)
	}
}
