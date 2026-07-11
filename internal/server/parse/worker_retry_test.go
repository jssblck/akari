package parse

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/storetest"
)

// TestWorkerRunSchedulesElapsedBackoffWithoutMaintenance proves that retry
// expiry is an event in its own right. No settle ticker and no explicit Wake is
// available after startup, so only the retry timer can rebuild the parked row.
func TestWorkerRunSchedulesElapsedBackoffWithoutMaintenance(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sid := seedSession(t, st, "worker-timed-retry")
	whole := claudeLines[0] + claudeLines[1] + claudeLines[2]
	if _, err := st.AppendChunk(ctx, sid, 0, []byte(whole)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE session_raw
		    SET parse_retry_at = now() + interval '200 milliseconds',
		        parse_retry_backoff_secs = 30
		  WHERE session_id = $1`, sid); err != nil {
		t.Fatalf("park due session: %v", err)
	}

	w := NewWorker(st, 1, 0)
	rebuilt := make(chan int64, 1)
	w.SetRebuiltHook(func(id int64) { rebuilt <- id })
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx)
	}()

	select {
	case id := <-rebuilt:
		if id != sid {
			t.Fatalf("rebuilt session %d, want %d", id, sid)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not retry after parse_retry_at elapsed")
	}
	if got := messageCount(t, st, sid); got != 2 {
		t.Fatalf("message_count = %d, want 2 after timed retry", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
}

func TestArmRetryTimerFallsBackAfterUnparkedDrainFailure(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	w := NewWorker(st, 1, 0)
	timer := time.NewTimer(time.Hour)
	stopTimer(timer)
	defer timer.Stop()

	// There are no parse_retry_at rows. A failed due scan still needs a future
	// attempt when maintenance is disabled, rather than a nil channel that can
	// leave pending work asleep forever.
	if ch := w.armRetryTimer(context.Background(), timer, true); ch == nil {
		t.Fatal("drain failure with no parked retry did not arm fallback timer")
	}
	if ch := w.armRetryTimer(context.Background(), timer, false); ch != nil {
		t.Fatal("successful drain with no parked retry unexpectedly armed timer")
	}
}
