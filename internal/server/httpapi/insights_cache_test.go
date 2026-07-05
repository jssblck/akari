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

// TestInsightsCacheReusesWithinTTL confirms a second read inside the TTL window
// serves the cached snapshot and does not recompute.
func TestInsightsCacheReusesWithinTTL(t *testing.T) {
	c := newInsightsCache()
	var calls atomic.Int64
	compute := func(context.Context) (store.Insights, error) {
		calls.Add(1)
		return store.Insights{Quality: store.QualityDistribution{Sessions: 1}}, nil
	}

	t0 := time.Unix(1_700_000_000, 0)
	if _, err := c.load(context.Background(), "year", t0, compute); err != nil {
		t.Fatalf("first load: %v", err)
	}
	if _, err := c.load(context.Background(), "year", t0.Add(insightsTTL-time.Second), compute); err != nil {
		t.Fatalf("second load: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 compute within TTL, got %d", got)
	}
}

// TestInsightsCacheExpiresAfterTTL confirms a read past the TTL recomputes.
func TestInsightsCacheExpiresAfterTTL(t *testing.T) {
	c := newInsightsCache()
	var calls atomic.Int64
	compute := func(context.Context) (store.Insights, error) {
		calls.Add(1)
		return store.Insights{}, nil
	}

	t0 := time.Unix(1_700_000_000, 0)
	_, _ = c.load(context.Background(), "year", t0, compute)
	_, _ = c.load(context.Background(), "year", t0.Add(insightsTTL+time.Second), compute)
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 computes across TTL boundary, got %d", got)
	}
}

// TestInsightsCacheKeyedByRange confirms distinct windows do not share an entry.
func TestInsightsCacheKeyedByRange(t *testing.T) {
	c := newInsightsCache()
	var calls atomic.Int64
	compute := func(context.Context) (store.Insights, error) {
		calls.Add(1)
		return store.Insights{}, nil
	}
	t0 := time.Unix(1_700_000_000, 0)
	_, _ = c.load(context.Background(), "year", t0, compute)
	_, _ = c.load(context.Background(), "30d", t0, compute)
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected a compute per distinct range, got %d", got)
	}
}

// TestInsightsCacheErrorNotCached confirms a failed compute is not stored, so the
// next reader retries rather than being pinned to the error for the whole TTL.
func TestInsightsCacheErrorNotCached(t *testing.T) {
	c := newInsightsCache()
	var calls atomic.Int64
	boom := errors.New("boom")
	compute := func(context.Context) (store.Insights, error) {
		if calls.Add(1) == 1 {
			return store.Insights{}, boom
		}
		return store.Insights{}, nil
	}
	t0 := time.Unix(1_700_000_000, 0)
	if _, err := c.load(context.Background(), "year", t0, compute); !errors.Is(err, boom) {
		t.Fatalf("expected boom, got %v", err)
	}
	if _, err := c.load(context.Background(), "year", t0, compute); err != nil {
		t.Fatalf("retry after error: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected the failed compute to be retried, got %d calls", got)
	}
}

// TestInsightsCacheCoalescesConcurrentMisses confirms a burst of cold readers on
// one key runs the compute once, not once per caller.
func TestInsightsCacheCoalescesConcurrentMisses(t *testing.T) {
	c := newInsightsCache()
	var calls atomic.Int64
	release := make(chan struct{})
	compute := func(context.Context) (store.Insights, error) {
		calls.Add(1)
		<-release // hold the flight open so every caller queues behind it
		return store.Insights{}, nil
	}
	t0 := time.Unix(1_700_000_000, 0)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.load(context.Background(), "year", t0, compute)
		}()
	}
	// Give the goroutines time to converge on the singleflight before releasing.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected concurrent misses to coalesce into 1 compute, got %d", got)
	}
}
