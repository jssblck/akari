//go:build unix

package discover

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDiscoverAcceptsOnlyRegularTargets(t *testing.T) {
	root := t.TempDir()
	regular := filepath.Join(root, "regular.jsonl")
	if err := os.WriteFile(regular, []byte("regular\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("linked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkedRegular := filepath.Join(root, "linked.jsonl")
	if err := os.Symlink(target, linkedRegular); err != nil {
		t.Fatal(err)
	}

	fifo := filepath.Join(root, "pipe.jsonl")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fifo, filepath.Join(root, "linked-pipe.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(os.DevNull, filepath.Join(root, "linked-device.jsonl")); err != nil {
		t.Fatal(err)
	}

	files, err := Discover([]Root{{Agent: "claude", Dir: root}}, Excluder{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{regular: true, linkedRegular: true}
	if len(files) != len(want) {
		t.Fatalf("discovered %d files, want %d regular targets: %+v", len(files), len(want), files)
	}
	for _, file := range files {
		if !want[file.Path] {
			t.Fatalf("discovered non-regular target %s", file.Path)
		}
	}
}
