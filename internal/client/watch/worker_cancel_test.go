package watch

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/client/discover"
	"github.com/jssblck/akari/internal/client/syncer"
)

func TestWorkerDoesNotStartQueuedSyncAfterCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	file := discover.File{Agent: "claude", Path: "queued.jsonl"}
	rs := &runState{
		w: &Watcher{sync: func(context.Context, discover.File) syncer.Result {
			started <- struct{}{}
			return syncer.Result{}
		}, opt: Options{Logf: func(string, ...any) {}}},
		dirty: map[discover.File]struct{}{file: {}},
		wake:  make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	// Hold the dirty-set lock until the worker has consumed its wake. Cancellation
	// then lands while it is committed to that wake but before it can pop work.
	rs.mu.Lock()
	go func() {
		rs.worker(ctx)
		close(done)
	}()
	rs.wake <- struct{}{}
	cancel()
	rs.mu.Unlock()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
	select {
	case <-started:
		t.Fatal("worker started a queued sync after cancellation")
	default:
	}
}
