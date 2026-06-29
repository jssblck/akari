package reparse

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/migrations"
)

// newTestStore mirrors the store/parse harnesses: it connects to
// AKARI_TEST_DATABASE_URL, resets the schema, and applies migrations, skipping
// when the env var is unset.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("AKARI_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AKARI_TEST_DATABASE_URL to run reparse integration tests")
	}
	ctx := context.Background()
	if err := store.EnsureDatabase(ctx, url); err != nil {
		t.Fatalf("ensure database: %v", err)
	}
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, q := range []string{"DROP SCHEMA public CASCADE", "CREATE SCHEMA public"} {
		if _, err := st.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("reset schema (%s): %v", q, err)
		}
	}
	if err := st.Migrate(ctx, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// TestRunStampsEpoch confirms a completed full reparse records parse.Epoch, so the
// next startup sees the epochs match and does not reparse again. An empty corpus is
// enough: the service still runs the loop (over zero sessions) and writes the epoch.
func TestRunStampsEpoch(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Sanity: a fresh database is "stale" (0 != parse.Epoch), which is what makes a
	// brand-new server reparse on first boot.
	if epoch, err := st.ReparsedEpoch(ctx); err != nil || epoch == parse.Epoch {
		t.Fatalf("fresh epoch = %d (err %v), want a value != parse.Epoch (%d)", epoch, err, parse.Epoch)
	}

	svc := New(ctx, st)
	res, err := svc.Run(ctx, Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.InProgress {
		t.Fatal("status should not be in progress after Run returns")
	}

	epoch, err := st.ReparsedEpoch(ctx)
	if err != nil {
		t.Fatalf("read epoch: %v", err)
	}
	if epoch != parse.Epoch {
		t.Fatalf("reparsed_epoch = %d, want parse.Epoch (%d)", epoch, parse.Epoch)
	}
}

// TestAgentFilteredRunDoesNotStampEpoch confirms a partial (agent-filtered) reparse
// leaves the epoch behind, so a targeted CLI run never claims the whole corpus is
// current.
func TestAgentFilteredRunDoesNotStampEpoch(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	svc := New(ctx, st)
	if _, err := svc.Run(ctx, Options{Agent: "claude"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if epoch, err := st.ReparsedEpoch(ctx); err != nil || epoch != 0 {
		t.Fatalf("epoch after agent-filtered run = %d (err %v), want 0 (unchanged)", epoch, err)
	}
}

// TestTriggerSerialized confirms a reparse already in progress makes a second
// Trigger a no-op that returns the running status rather than starting a second
// run. The first run is held inside its initial progress emit so the window stays
// open deterministically.
func TestTriggerSerialized(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	svc := New(ctx, st)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	svc.SetProgressHook(func(Status) {
		// Block the very first emit (the start of execute, before any work), holding
		// InProgress=true for the duration of the test.
		once.Do(func() {
			close(started)
			<-release
		})
	})

	svc.Trigger(Options{})
	<-started

	if !svc.Status().InProgress {
		t.Fatal("status should be in progress while the first run is held")
	}
	// A second Trigger must not start a new run; it returns the running status.
	if got := svc.Trigger(Options{}); !got.InProgress {
		t.Fatal("second Trigger should return the running status (in progress)")
	}

	close(release)
	svc.Wait()

	if svc.Status().InProgress {
		t.Fatal("status should clear after the run completes")
	}
	if epoch, err := st.ReparsedEpoch(ctx); err != nil || epoch != parse.Epoch {
		t.Fatalf("epoch after completion = %d (err %v), want parse.Epoch (%d)", epoch, err, parse.Epoch)
	}
}

// TestSkipsWhenLockHeld confirms the service skips (and does not stamp the epoch)
// when another holder owns the advisory lock, the cross-process guard that keeps
// two instances from reparsing at once.
func TestSkipsWhenLockHeld(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	// Stand in for another instance by holding the lock on a separate connection.
	held, ok, err := st.AcquireReparseLock(ctx)
	if err != nil || !ok {
		t.Fatalf("pre-hold lock: ok=%v err=%v", ok, err)
	}
	defer held.Release(ctx)

	svc := New(ctx, st)
	if _, err := svc.Run(ctx, Options{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if svc.Status().InProgress {
		t.Fatal("status should be cleared after a skipped run")
	}
	if epoch, err := st.ReparsedEpoch(ctx); err != nil || epoch != 0 {
		t.Fatalf("epoch after skipped run = %d (err %v), want 0 (lock held, no reparse)", epoch, err)
	}
}
