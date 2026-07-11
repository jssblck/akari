//go:build windows

package watch

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/client/discover"
)

// mkjunction creates a Windows directory junction, mirroring
// discover.mkjunction: `mklink /J` needs no elevated privilege, unlike a real
// directory symlink, which is why the package's junction regression coverage
// uses it rather than a symlink.
func mkjunction(t *testing.T, link, target string) {
	t.Helper()
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf("mklink /J %s -> %s: %v: %s", link, target, err, out)
	}
}

// TestWatchRejectsJunctionRootByDefault proves watch shares discover's closed
// root-link policy rather than having its own copy that could drift: a
// non-Optional junction root must fail addRecursive (logged once from the
// initial pass) and never sync a file placed under it, the same as
// discover.Discover would reject it outright.
func TestWatchRejectsJunctionRootByDefault(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	writeSession(t, filepath.Join(target, "session.jsonl"))
	link := filepath.Join(dir, "link")
	mkjunction(t, link, target)

	fn, synced := recorder()
	logs := make(chan string, 8)
	opt := fastOptions()
	opt.Logf = func(format string, args ...any) {
		logs <- fmt.Sprintf(format, args...)
	}
	w := New([]discover.Root{{Agent: "claude", Dir: link}}, fn, opt)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	select {
	case logLine := <-logs:
		if !strings.Contains(logLine, "directory junction") {
			t.Fatalf("watch log = %q, want it to name the directory junction", logLine)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not report the rejected junction root")
	}
	select {
	case got := <-synced:
		t.Fatalf("watch synced a file through a rejected junction root: %s", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestWatchSkipsOptionalJunctionRootQuietly proves the built-in-root exception
// holds in watch too: an Optional junction root produces a notice, not an error,
// and the watcher keeps running cleanly.
func TestWatchSkipsOptionalJunctionRootQuietly(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	writeSession(t, filepath.Join(target, "session.jsonl"))
	link := filepath.Join(dir, "link")
	mkjunction(t, link, target)

	fn, synced := recorder()
	logs := make(chan string, 8)
	opt := fastOptions()
	opt.Logf = func(format string, args ...any) {
		logs <- fmt.Sprintf(format, args...)
	}
	w := New([]discover.Root{{Agent: "claude", Dir: link, Optional: true}}, fn, opt)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	select {
	case logLine := <-logs:
		if !strings.Contains(logLine, "directory junction") || !strings.Contains(logLine, "skipped") {
			t.Fatalf("watch log = %q, want a skip notice naming the junction", logLine)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not report the skipped Optional junction root")
	}
	select {
	case got := <-synced:
		t.Fatalf("watch synced a file through a skipped Optional junction root: %s", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestWatchFollowsJunctionRootWhenOptedIn proves the opt-in works end to end
// through watch, not just discover.Discover: with FollowRootLink set, the
// initial pass syncs the file already sitting under the junction's target, and a
// file created afterward is picked up too, proving addRecursive actually
// attached an fsnotify watch to the resolved directory (not the link, which
// fsnotify cannot watch meaningfully) rather than only satisfying the one-shot
// discovery walk.
func TestWatchFollowsJunctionRootWhenOptedIn(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	existing := filepath.Join(target, "existing.jsonl")
	writeSession(t, existing)
	link := filepath.Join(dir, "link")
	mkjunction(t, link, target)

	fn, synced := recorder()
	w := New([]discover.Root{{Agent: "claude", Dir: link, FollowRootLink: true}}, fn, fastOptions())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	waitFor(t, synced, existing)

	created := filepath.Join(target, "created.jsonl")
	writeSession(t, created)
	waitFor(t, synced, created)
}
