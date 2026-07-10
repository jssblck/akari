//go:build unix

package watch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jssblck/akari/internal/client/discover"
	"golang.org/x/sys/unix"
)

func TestFileForAcceptsOnlyRegularTargets(t *testing.T) {
	root := t.TempDir()
	w := New([]discover.Root{{Agent: "claude", Dir: root}}, nil, Options{})

	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("session\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkedRegular := filepath.Join(root, "linked.jsonl")
	if err := os.Symlink(target, linkedRegular); err != nil {
		t.Fatal(err)
	}
	if _, ok := w.fileFor(linkedRegular); !ok {
		t.Fatal("symlink to a regular session was rejected")
	}

	fifo := filepath.Join(root, "pipe.jsonl")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	linkedFIFO := filepath.Join(root, "linked-pipe.jsonl")
	if err := os.Symlink(fifo, linkedFIFO); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{fifo, linkedFIFO} {
		if _, ok := w.fileFor(path); ok {
			t.Fatalf("classified non-regular target %s as a session", path)
		}
	}
}
