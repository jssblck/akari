package watch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/client/discover"
)

func TestWatchReaddsRecreatedDirectory(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "project")
	initial := filepath.Join(dir, "initial.jsonl")
	writeSession(t, initial)

	fn, synced := recorder()
	w := New([]discover.Root{{Agent: "claude", Dir: root}}, fn, Options{
		Debounce: 10 * time.Millisecond,
		Poll:     time.Hour,
		Discover: time.Hour,
		Rescan:   time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("watcher did not stop")
		}
	})

	waitFor(t, synced, initial)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Keep creating distinct sessions until the directory recreation event has
	// been handled. Discovery and polling are disabled, so a sync proves fsnotify
	// was attached to the new directory object.
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	created := 0
	for {
		select {
		case got := <-synced:
			if got != initial && filepath.Dir(got) == dir {
				return
			}
		case <-ticker.C:
			path := filepath.Join(dir, fmt.Sprintf("after-%d.jsonl", created))
			created++
			writeSession(t, path)
		case <-deadline.C:
			t.Fatal("recreated directory never regained an fsnotify watch")
		}
	}
}
