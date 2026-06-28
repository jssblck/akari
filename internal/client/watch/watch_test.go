package watch

import (
	"context"
	"os"
	"path/filepath"
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
	return Options{Debounce: 20 * time.Millisecond, Poll: 50 * time.Millisecond, Rescan: time.Hour}
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
