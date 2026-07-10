//go:build !windows

package resolve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/client/discover"
	"golang.org/x/sys/unix"
)

func TestPeekHeaderRejectsFIFOWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "session.jsonl")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	fifoLink := filepath.Join(dir, "linked-session.jsonl")
	if err := os.Symlink(fifo, fifoLink); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{fifo, fifoLink} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				_, err := PeekHeader(discover.File{Agent: "claude", Root: dir, Path: path})
				done <- err
			}()

			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "not a regular file") {
					t.Fatalf("PeekHeader FIFO error = %v", err)
				}
			case <-time.After(time.Second):
				// This writer releases a reader stuck in an accidental blocking open,
				// keeping the failed regression test from leaking a goroutine.
				if fd, err := unix.Open(fifo, unix.O_WRONLY|unix.O_NONBLOCK, 0); err == nil {
					_ = unix.Close(fd)
				}
				t.Fatal("PeekHeader blocked opening a FIFO")
			}
		})
	}
}

func TestPeekHeaderFollowsSymlinkToRegularFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.jsonl")
	line := `{"type":"user","cwd":"/home/ada/project","message":{"content":"hi"}}` + "\n"
	if err := os.WriteFile(target, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	header, err := PeekHeader(discover.File{Agent: "claude", Root: dir, Path: link})
	if err != nil {
		t.Fatal(err)
	}
	if header.Cwd != "/home/ada/project" {
		t.Fatalf("cwd = %q, want /home/ada/project", header.Cwd)
	}
}
