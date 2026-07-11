//go:build unix

package discover

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDiscoverRejectsSymlinksAndNonRegularFiles(t *testing.T) {
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

	files, _, err := Discover([]Root{{Agent: "claude", Dir: root}}, Excluder{})
	if ErrorCount(err) != 3 {
		t.Fatalf("ErrorCount = %d, want 3 symlink errors: %v", ErrorCount(err), err)
	}
	if len(files) != 1 || files[0].Path != regular {
		t.Fatalf("files = %+v, want only regular file", files)
	}
}
