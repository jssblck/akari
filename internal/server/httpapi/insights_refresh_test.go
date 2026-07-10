package httpapi

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// snapshotOf builds a one-generation compute result whose ranges are
// distinguishable by session count, so a test can tell which generation a get
// served and that two ranges came from the same pass.
func snapshotOf(gen int) map[string]store.Insights {
	return map[string]store.Insights{
		"7d":   {Quality: store.QualityDistribution{Sessions: gen}},
		"year": {Quality: store.QualityDistribution{Sessions: gen}},
	}
}

// TestInsightsRefresherServesSnapshotWithoutRecompute confirms reads serve the
// installed snapshot regardless of age: one cold pass, then every later get for any
// range is a lookup that reports the same computed-at instant. Freshness is the
// background loop's job, not the reader's.
func TestInsightsRefresherServesSnapshotWithoutRecompute(t *testing.T) {
	var calls atomic.Int64
	r := newInsightsRefresher(func(context.Context) (map[string]store.Insights, error) {
		calls.Add(1)
		return snapshotOf(1), nil
	})

	_, at7, err := r.get(context.Background(), "7d")
	if err != nil {
		t.Fatalf("cold get: %v", err)
	}
	_, atY, err := r.get(context.Background(), "year")
	if err != nil {
		t.Fatalf("warm get: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected one pass across both ranges, got %d", got)
	}
	if !at7.Equal(atY) {
		t.Errorf("ranges report different computed-at instants (%s vs %s); they must come from one pass", at7, atY)
	}
}

// TestInsightsRefresherUnknownRange confirms a range key outside the computed set is
// a loud error (a wiring bug), not a silent empty page.
func TestInsightsRefresherUnknownRange(t *testing.T) {
	r := newInsightsRefresher(func(context.Context) (map[string]store.Insights, error) {
		return snapshotOf(1), nil
	})
	if _, _, err := r.get(context.Background(), "fortnight"); err == nil {
		t.Error("expected an error for a range the pass does not compute")
	}
}

// TestInsightsRefresherColdGetsCoalesce confirms a burst of cold readers runs one
// pass, not one per caller.
func TestInsightsRefresherColdGetsCoalesce(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	r := newInsightsRefresher(func(context.Context) (map[string]store.Insights, error) {
		calls.Add(1)
		<-release // hold the pass open so every caller queues behind it
		return snapshotOf(1), nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = r.get(context.Background(), "7d")
		}()
	}
	// Give the goroutines time to converge on the singleflight before releasing.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected concurrent cold gets to coalesce into 1 pass, got %d", got)
	}
}

// TestInsightsRefresherErrorKeepsPreviousSnapshot confirms a failed pass installs
// nothing: readers keep the previous generation, and the failure is retried on the
// next trigger rather than pinned.
func TestInsightsRefresherErrorKeepsPreviousSnapshot(t *testing.T) {
	var calls atomic.Int64
	boom := errors.New("boom")
	r := newInsightsRefresher(func(context.Context) (map[string]store.Insights, error) {
		switch calls.Add(1) {
		case 2:
			return nil, boom
		default:
			return snapshotOf(int(calls.Load())), nil
		}
	})

	if _, _, err := r.get(context.Background(), "7d"); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if err := r.refresh(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("second pass should fail with boom, got %v", err)
	}
	ins, _, err := r.get(context.Background(), "7d")
	if err != nil {
		t.Fatalf("get after failed pass: %v", err)
	}
	if ins.Quality.Sessions != 1 {
		t.Errorf("get served generation %d, want 1 (the failed pass must not clobber the snapshot)", ins.Quality.Sessions)
	}
	if err := r.refresh(context.Background()); err != nil {
		t.Fatalf("third pass should retry cleanly, got %v", err)
	}
	if ins, _, _ := r.get(context.Background(), "7d"); ins.Quality.Sessions != 3 {
		t.Errorf("get served generation %d, want 3 after the retry", ins.Quality.Sessions)
	}
}

// TestInsightsRefresherColdErrorRetries confirms a cold start whose pass fails does
// not cache the failure: the next reader triggers a fresh pass.
func TestInsightsRefresherColdErrorRetries(t *testing.T) {
	var calls atomic.Int64
	boom := errors.New("boom")
	r := newInsightsRefresher(func(context.Context) (map[string]store.Insights, error) {
		if calls.Add(1) == 1 {
			return nil, boom
		}
		return snapshotOf(2), nil
	})
	if _, _, err := r.get(context.Background(), "7d"); !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}
	if _, _, err := r.get(context.Background(), "7d"); err != nil {
		t.Fatalf("retry after error: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected the failed pass to be retried, got %d calls", got)
	}
}

// TestInsightsRefresherCallerCancelReturns confirms a caller whose context is
// canceled while the shared pass is in flight returns promptly with its context
// error rather than staying parked. The pass keeps running and installs the
// snapshot, so a later reader gets the value with no second pass.
func TestInsightsRefresherCallerCancelReturns(t *testing.T) {
	var calls atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	r := newInsightsRefresher(func(context.Context) (map[string]store.Insights, error) {
		calls.Add(1)
		close(started)
		<-release // hold the pass open past the first caller's cancellation
		return snapshotOf(7), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	gaveUp := make(chan error, 1)
	go func() {
		_, _, err := r.get(ctx, "7d")
		gaveUp <- err
	}()
	<-started
	cancel()
	select {
	case err := <-gaveUp:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a canceled caller should return context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a canceled caller stayed parked instead of returning")
	}

	// A second caller coalesces onto the still-running pass; releasing it installs
	// the snapshot and hands this waiter the value from that one run.
	warm := make(chan store.Insights, 1)
	go func() {
		ins, _, _ := r.get(context.Background(), "7d")
		warm <- ins
	}()
	time.Sleep(20 * time.Millisecond) // let the second caller attach to the in-flight pass
	close(release)
	if ins := <-warm; ins.Quality.Sessions != 7 {
		t.Errorf("warm read = %d sessions, want 7 (the detached pass's value)", ins.Quality.Sessions)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected one coalesced pass across the canceled and warm callers, got %d", got)
	}
}

// TestInsightsRefresherRunKickAndBusy drives the background loop: with the proactive
// cadence disabled (interval 0) nothing computes until a kick; a kick that lands
// while a fleet drain is in progress is skipped; a kick after the drain recomputes.
func TestInsightsRefresherRunKickAndBusy(t *testing.T) {
	var calls atomic.Int64
	computed := make(chan struct{}, 8)
	r := newInsightsRefresher(func(context.Context) (map[string]store.Insights, error) {
		calls.Add(1)
		computed <- struct{}{}
		return snapshotOf(int(calls.Load())), nil
	})

	var busy atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.run(ctx, 0, busy.Load)
	}()

	// Interval 0: no warm pass. Give the loop a beat, then confirm nothing ran.
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("disabled cadence ran %d passes before any kick", got)
	}

	// A kick mid-drain is skipped: the corpus is half rewritten.
	busy.Store(true)
	r.kickRefresh()
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("busy kick ran %d passes, want 0", got)
	}

	// The drain finishes and the completion kick recomputes.
	busy.Store(false)
	r.kickRefresh()
	select {
	case <-computed:
	case <-time.After(2 * time.Second):
		t.Fatal("kick after drain never recomputed")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not stop on context cancellation")
	}
}

// TestInsightsRefresherRunTicks confirms the proactive cadence recomputes on its own:
// a warm pass at startup, then a pass per tick.
func TestInsightsRefresherRunTicks(t *testing.T) {
	var calls atomic.Int64
	computed := make(chan struct{}, 64)
	r := newInsightsRefresher(func(context.Context) (map[string]store.Insights, error) {
		calls.Add(1)
		computed <- struct{}{}
		return snapshotOf(int(calls.Load())), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.run(ctx, 5*time.Millisecond, nil)
	}()

	// The startup warm pass plus at least one tick-driven pass.
	for i := 0; i < 2; i++ {
		select {
		case <-computed:
		case <-time.After(2 * time.Second):
			t.Fatalf("saw only %d passes, want at least 2 (warm + tick)", i)
		}
	}
	cancel()
	<-done
}
