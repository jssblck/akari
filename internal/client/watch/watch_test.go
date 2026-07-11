package watch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/client/discover"
	"github.com/jssblck/akari/internal/client/syncer"
)

// recorder is a SyncFunc that reports each synced path on a channel.
func recorder() (SyncFunc, chan string) {
	ch := make(chan string, 64)
	fn := func(_ context.Context, f discover.File) syncer.Result {
		select {
		case ch <- f.Path:
		default:
		}
		return syncer.Result{File: f}
	}
	return fn, ch
}

func TestWatchReportsIncompleteDiscoveryAndSyncsSafeFiles(t *testing.T) {
	root := t.TempDir()
	safe := filepath.Join(root, "safe.jsonl")
	target := filepath.Join(root, "target.txt")
	linked := filepath.Join(root, "linked.jsonl")
	writeSession(t, safe)
	writeSession(t, target)
	if err := os.Symlink(target, linked); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	fn, synced := recorder()
	logs := make(chan string, 8)
	opt := fastOptions()
	opt.Discover = time.Hour
	opt.Logf = func(format string, args ...any) {
		logs <- fmt.Sprintf(format, args...)
	}
	w := New([]discover.Root{{Agent: "claude", Dir: root}}, fn, opt)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	waitFor(t, synced, safe)
	select {
	case got := <-synced:
		if got == linked {
			t.Fatalf("watch synced symlink %s", got)
		}
	default:
	}
	select {
	case logLine := <-logs:
		if !strings.Contains(logLine, "discovery incomplete (1 error(s))") || !strings.Contains(logLine, linked) {
			t.Fatalf("watch log = %q", logLine)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not report the discovery error")
	}
}

// TestWatchDedupsRepeatedDiscoveryFailure guards against the log spam a standing
// discovery failure used to cause: with the discover ticker firing every 15ms,
// an unchanging failure must still log only once, not once per tick, and fixing
// the failure must log the recovery exactly once too rather than trailing off
// silently or double-logging.
func TestWatchDedupsRepeatedDiscoveryFailure(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "does-not-exist")

	fn, _ := recorder()
	logs := make(chan string, 256)
	opt := Options{
		Debounce: 10 * time.Millisecond,
		Poll:     time.Hour,
		Discover: 15 * time.Millisecond,
		Rescan:   time.Hour,
		Logf: func(format string, args ...any) {
			logs <- fmt.Sprintf(format, args...)
		},
	}
	// A required (non-Optional) missing root is a discovery error on every pass
	// until it exists, the portable way to hold a failure's text perfectly
	// constant across ticks without relying on symlinks or permission fixtures.
	w := New([]discover.Root{{Agent: "claude", Dir: missing}}, fn, opt)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	// Several discover ticks elapse with the identical failure standing. The
	// initial pass at startup also logs once via addRecursive's own separate
	// "watch root ..." line (a different, one-time message, not the repeating
	// discover-tick spam this test guards against), so only count the
	// "discovery incomplete" line the discover ticker's w.discover() calls emit.
	time.Sleep(200 * time.Millisecond)
	if n := drainLogCountMatching(logs, "discovery incomplete"); n != 1 {
		t.Fatalf("logged the standing discovery failure %d time(s), want exactly 1", n)
	}

	// Fixing the root changes discovery from failing to healthy: that must log
	// the recovery exactly once, even though several more healthy ticks follow.
	if err := os.MkdirAll(missing, 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := drainLogCountMatching(logs, "discovery recovered"); n != 1 {
		t.Fatalf("logged the recovery %d time(s), want exactly 1", n)
	}
}

func drainLogCountMatching(ch chan string, substr string) int {
	n := 0
	for {
		select {
		case line := <-ch:
			if strings.Contains(line, substr) {
				n++
			}
		default:
			return n
		}
	}
}

func writeSession(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"type":"session"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, ch chan string, want string) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case got := <-ch:
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for sync of %s", want)
		}
	}
}

func fastOptions() Options {
	// Discover is the fallback that finds newly created files when no fsnotify
	// Create event is delivered (a freshly created subdirectory races the watch
	// add); keep it fast here so the new-file test exercises that fallback
	// deterministically rather than depending on the event race.
	return Options{Debounce: 20 * time.Millisecond, Poll: 50 * time.Millisecond, Discover: 50 * time.Millisecond, Rescan: time.Hour}
}

func TestWatchInitialPass(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proj", "sess.jsonl")
	writeSession(t, path)

	fn, ch := recorder()
	w := New([]discover.Root{{Agent: "claude", Dir: dir}}, fn, fastOptions())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	waitFor(t, ch, path) // the existing file is synced on the initial pass
}

func TestWatchDetectsNewFile(t *testing.T) {
	dir := t.TempDir()
	fn, ch := recorder()
	w := New([]discover.Root{{Agent: "claude", Dir: dir}}, fn, fastOptions())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// Give the watcher a moment to establish watches, then create a file.
	time.Sleep(100 * time.Millisecond)
	path := filepath.Join(dir, "proj", "new.jsonl")
	writeSession(t, path)

	waitFor(t, ch, path)
}

func TestWatchHonorsExcludes(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "proj", "keep.jsonl")
	excluded := filepath.Join(dir, "proj", "dropme", "drop.jsonl")
	writeSession(t, keep)
	writeSession(t, excluded)

	fn, ch := recorder()
	opt := fastOptions()
	// "dropme", not "tmp": t.TempDir() is under /tmp on Linux, so **/tmp/** would
	// exclude the kept file too and the test would pass for the wrong reason.
	opt.Excludes = []string{"**/dropme/**"}
	w := New([]discover.Root{{Agent: "claude", Dir: dir}}, fn, opt)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	// The kept file must sync; the excluded one must never appear. Drain for a
	// window long enough for the initial pass and a few discover/poll ticks.
	sawKeep := false
	deadline := time.After(1 * time.Second)
	for {
		select {
		case got := <-ch:
			if got == excluded {
				t.Fatalf("excluded file was synced: %s", got)
			}
			if got == keep {
				sawKeep = true
			}
		case <-deadline:
			if !sawKeep {
				t.Fatal("kept file was never synced")
			}
			return
		}
	}
}

func TestWatchIgnoresNonSessionFiles(t *testing.T) {
	dir := t.TempDir()
	fn, ch := recorder()
	w := New([]discover.Root{{Agent: "codex", Dir: dir}}, fn, fastOptions())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.Run(ctx)

	time.Sleep(100 * time.Millisecond)
	// For codex, only rollout-*.jsonl count; a plain .jsonl must be ignored.
	writeSession(t, filepath.Join(dir, "notes.jsonl"))
	rollout := filepath.Join(dir, "rollout-abc.jsonl")
	writeSession(t, rollout)

	// The rollout file should sync; if the watcher were misclassifying, notes
	// would arrive first. Drain until we see the rollout, failing on notes.
	deadline := time.After(3 * time.Second)
	for {
		select {
		case got := <-ch:
			if got == rollout {
				return
			}
			if filepath.Base(got) == "notes.jsonl" {
				t.Fatalf("non-session file was synced: %s", got)
			}
		case <-deadline:
			t.Fatal("timed out waiting for rollout sync")
		}
	}
}
