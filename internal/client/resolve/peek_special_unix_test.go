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

	// The direct FIFO fails the regular-file check; the symlinked FIFO is
	// rejected one step earlier by the closed symlink policy, before its
	// target's type is ever consulted. Both must fail without blocking.
	wantErr := map[string]string{fifo: "not a regular file", fifoLink: "symlink"}
	for _, path := range []string{fifo, fifoLink} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				_, err := PeekHeader(discover.File{Agent: "claude", Root: dir, Path: path})
				done <- err
			}()

			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), wantErr[path]) {
					t.Fatalf("PeekHeader FIFO error = %v, want %q", err, wantErr[path])
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

// TestPeekHeaderRejectsSymlinkTarget guards the closed symlink policy at read
// time: discover.Discover already refuses to hand PeekHeader a symlink path in
// the ordinary flow, but PeekHeader must not silently follow one if it is ever
// called directly on one (a caller error, or a path that was swapped for a
// symlink after discovery approved it; see TestPeekHeaderRejectsSwappedSymlink).
func TestPeekHeaderRejectsSymlinkTarget(t *testing.T) {
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

	_, err := PeekHeader(discover.File{Agent: "claude", Root: dir, Path: link})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("PeekHeader(symlink) err = %v, want a symlink rejection", err)
	}
}

// TestPeekHeaderRejectsSwappedSymlink simulates the realistic time-of-check to
// time-of-use race the closed policy has to close: discover.Discover Lstats a
// path and finds an ordinary regular file, but before resolve.PeekHeader gets to
// read it, the path is replaced with a symlink to a file outside the discovered
// root. A plain Stat-then-Open would follow that symlink and upload the outside
// file's content under the discovered session's identity; PeekHeader's own
// Lstat-first check must reject it instead.
func TestPeekHeaderRejectsSwappedSymlink(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte(`{"type":"user","cwd":"/somewhere/else","message":{"content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","cwd":"/home/ada/project","message":{"content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The swap: discovery would have already approved `path` as a regular file;
	// this replaces it with a symlink to `outside` before PeekHeader reads it.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}

	_, err := PeekHeader(discover.File{Agent: "claude", Root: dir, Path: path})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("PeekHeader(swapped symlink) err = %v, want a symlink rejection", err)
	}
}
