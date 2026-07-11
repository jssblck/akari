package httpapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

func snapshotWithSessions(n int) analyticsPageSnapshot {
	return analyticsPageSnapshot{analytics: store.Analytics{Sessions: n}}
}

func quietSnapshotCache(freshFor, staleFor time.Duration, limit int, compute analyticsSnapshotCompute) *analyticsSnapshotCache {
	c := newAnalyticsSnapshotCache(freshFor, staleFor, limit, compute)
	c.logf = func(string, ...any) {}
	return c
}

func TestAnalyticsSnapshotCacheColdStartStampede(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	c := quietSnapshotCache(time.Minute, 15*time.Minute, 32, func(context.Context, analyticsSnapshotKey, time.Time) (analyticsPageSnapshot, error) {
		calls.Add(1)
		<-release
		return snapshotWithSessions(7), nil
	})

	key := analyticsSnapshotKey{scope: analyticsScope{kind: analyticsProjectScope, id: 11}, rangeKey: "year"}
	// Install a hook that closes release only after all callers attach.
	const callers = 64
	var attached sync.WaitGroup
	attached.Add(callers)
	c.afterFlight = attached.Done
	results := make([]analyticsPageSnapshot, callers)
	var wg sync.WaitGroup
	wg.Add(callers)
	start := make(chan struct{})
	for i := range callers {
		go func() {
			defer wg.Done()
			<-start
			results[i], _, _ = c.get(context.Background(), key)
		}()
	}
	close(start)
	attached.Wait()
	close(release)
	wg.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("cold burst ran %d refreshes, want 1", got)
	}
	for i, result := range results {
		if result.analytics.Sessions != 7 {
			t.Fatalf("caller %d got %d sessions, want 7", i, result.analytics.Sessions)
		}
	}
}

func TestAnalyticsSnapshotCacheRefreshStampede(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	c := quietSnapshotCache(time.Minute, 15*time.Minute, 32, func(context.Context, analyticsSnapshotKey, time.Time) (analyticsPageSnapshot, error) {
		generation := int(calls.Add(1))
		if generation == 2 {
			<-release
		}
		return snapshotWithSessions(generation), nil
	})
	c.now = func() time.Time { return now }
	key := analyticsSnapshotKey{scope: analyticsScope{kind: analyticsUserScope, id: 12}, rangeKey: "30d"}
	if _, _, err := c.get(context.Background(), key); err != nil {
		t.Fatalf("prime: %v", err)
	}
	now = now.Add(2 * time.Minute)

	const callers = 48
	var attached sync.WaitGroup
	attached.Add(callers)
	c.afterFlight = attached.Done
	var wg sync.WaitGroup
	wg.Add(callers)
	for range callers {
		go func() {
			defer wg.Done()
			_, _, _ = c.get(context.Background(), key)
		}()
	}
	attached.Wait()
	close(release)
	wg.Wait()
	if got := calls.Load(); got != 2 {
		t.Fatalf("expired burst ran %d total refreshes, want 2 (prime + one refresh)", got)
	}
}

func TestAnalyticsSnapshotCacheStaleOnErrorExpires(t *testing.T) {
	boom := errors.New("database unavailable")
	var calls atomic.Int64
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	c := quietSnapshotCache(time.Minute, 5*time.Minute, 8, func(context.Context, analyticsSnapshotKey, time.Time) (analyticsPageSnapshot, error) {
		if calls.Add(1) == 1 {
			return snapshotWithSessions(9), nil
		}
		return analyticsPageSnapshot{}, boom
	})
	c.now = func() time.Time { return now }
	key := analyticsSnapshotKey{scope: analyticsScope{kind: analyticsUserScope, id: 13}, rangeKey: "7d"}
	if _, _, err := c.get(context.Background(), key); err != nil {
		t.Fatalf("prime: %v", err)
	}

	now = now.Add(2 * time.Minute)
	snapshot, meta, err := c.get(context.Background(), key)
	if err != nil {
		t.Fatalf("stale fallback: %v", err)
	}
	if meta.state != analyticsSnapshotStale || snapshot.analytics.Sessions != 9 {
		t.Fatalf("fallback = (state %q, sessions %d), want (stale, 9)", meta.state, snapshot.analytics.Sessions)
	}

	now = now.Add(5 * time.Minute)
	if _, _, err := c.get(context.Background(), key); !errors.Is(err, boom) {
		t.Fatalf("past stale limit error = %v, want %v", err, boom)
	}
}

func TestAnalyticsSnapshotCacheMultipleScopesAndLRUBound(t *testing.T) {
	var calls atomic.Int64
	c := quietSnapshotCache(time.Hour, 0, 2, func(_ context.Context, key analyticsSnapshotKey, _ time.Time) (analyticsPageSnapshot, error) {
		calls.Add(1)
		return snapshotWithSessions(int(key.scope.id)), nil
	})
	keys := []analyticsSnapshotKey{
		{scope: analyticsScope{kind: analyticsProjectScope, id: 1}, rangeKey: "year"},
		{scope: analyticsScope{kind: analyticsProjectScope, id: 2}, rangeKey: "year"},
		{scope: analyticsScope{kind: analyticsProjectScope, id: 3}, rangeKey: "year"},
	}
	for _, key := range keys {
		if _, _, err := c.get(context.Background(), key); err != nil {
			t.Fatalf("get %s: %v", key, err)
		}
	}
	c.mu.Lock()
	entries := len(c.entries)
	_, firstRemains := c.entries[keys[0]]
	c.mu.Unlock()
	if entries != 2 || firstRemains {
		t.Fatalf("LRU = (entries %d, first remains %v), want (2, false)", entries, firstRemains)
	}
	if _, _, err := c.get(context.Background(), keys[0]); err != nil {
		t.Fatalf("reload evicted scope: %v", err)
	}
	if got := calls.Load(); got != 4 {
		t.Fatalf("three scopes plus one evicted reload ran %d computes, want 4", got)
	}
}

func TestAnalyticsSnapshotCacheRefreshesDifferentScopesIndependently(t *testing.T) {
	started := make(chan analyticsSnapshotKey, 2)
	release := make(chan struct{})
	var calls atomic.Int64
	c := quietSnapshotCache(time.Hour, 0, 8, func(_ context.Context, key analyticsSnapshotKey, _ time.Time) (analyticsPageSnapshot, error) {
		calls.Add(1)
		started <- key
		<-release
		return snapshotWithSessions(int(key.scope.id)), nil
	})
	keys := []analyticsSnapshotKey{
		{scope: analyticsScope{kind: analyticsProjectScope, id: 31}, rangeKey: "year"},
		{scope: analyticsScope{kind: analyticsProjectScope, id: 32}, rangeKey: "year"},
	}
	results := make(chan analyticsPageSnapshot, len(keys))
	for _, key := range keys {
		go func() {
			snapshot, _, _ := c.get(context.Background(), key)
			results <- snapshot
		}()
	}
	seen := map[analyticsSnapshotKey]bool{}
	for range keys {
		seen[<-started] = true
	}
	if !seen[keys[0]] || !seen[keys[1]] || calls.Load() != 2 {
		t.Fatalf("started scopes = %v with %d calls, want both scopes and 2 calls", seen, calls.Load())
	}
	close(release)
	got := map[int]bool{}
	for range keys {
		got[(<-results).analytics.Sessions] = true
	}
	if !got[31] || !got[32] {
		t.Fatalf("results = %v, want independent scope payloads 31 and 32", got)
	}
}

func TestAnalyticsSnapshotCacheInvalidationRejectsInflightInstall(t *testing.T) {
	var calls atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	c := quietSnapshotCache(time.Hour, 0, 8, func(context.Context, analyticsSnapshotKey, time.Time) (analyticsPageSnapshot, error) {
		generation := calls.Add(1)
		if generation == 1 {
			close(started)
			<-release
		}
		return snapshotWithSessions(int(generation)), nil
	})
	scope := analyticsScope{kind: analyticsProjectScope, id: 14}
	key := analyticsSnapshotKey{scope: scope, rangeKey: "year"}
	done := make(chan error, 1)
	go func() {
		_, _, err := c.get(context.Background(), key)
		done <- err
	}()
	<-started
	c.invalidate(scope)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("in-flight authorized request: %v", err)
	}
	c.mu.Lock()
	_, installed := c.entries[key]
	c.mu.Unlock()
	if installed {
		t.Fatal("refresh installed after invalidation")
	}
	snapshot, _, err := c.get(context.Background(), key)
	if err != nil {
		t.Fatalf("post-invalidation get: %v", err)
	}
	if snapshot.analytics.Sessions != 2 || calls.Load() != 2 {
		t.Fatalf("post-invalidation = (sessions %d, calls %d), want (2, 2)", snapshot.analytics.Sessions, calls.Load())
	}
}

func TestAnalyticsSnapshotCacheCanceledWaiterDoesNotCancelSharedRefresh(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int64
	c := quietSnapshotCache(time.Hour, 0, 8, func(context.Context, analyticsSnapshotKey, time.Time) (analyticsPageSnapshot, error) {
		calls.Add(1)
		close(started)
		<-release
		return snapshotWithSessions(21), nil
	})
	key := analyticsSnapshotKey{scope: analyticsScope{kind: analyticsUserScope, id: 15}, rangeKey: "year"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := c.get(ctx, key)
		done <- err
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v, want context.Canceled", err)
	}
	warm := make(chan analyticsPageSnapshot, 1)
	warmErr := make(chan error, 1)
	go func() {
		snapshot, _, err := c.get(context.Background(), key)
		warm <- snapshot
		warmErr <- err
	}()
	close(release)
	snapshot, err := <-warm, <-warmErr
	if err != nil || snapshot.analytics.Sessions != 21 || calls.Load() != 1 {
		t.Fatalf("warm get = (sessions %d, calls %d, err %v), want (21, 1, nil)", snapshot.analytics.Sessions, calls.Load(), err)
	}
}

func TestObserveAnalyticsSnapshotMarksStaleResponses(t *testing.T) {
	w := httptest.NewRecorder()
	started := time.Now().Add(-25 * time.Millisecond)
	observeAnalyticsSnapshot(w, started, analyticsSnapshotMeta{
		state: analyticsSnapshotStale,
		at:    time.Now().Add(-2 * time.Minute),
	}, time.Minute, 15*time.Minute)
	if got := w.Header().Get("Warning"); got != `110 - "Response is stale"` {
		t.Fatalf("Warning = %q, want stale warning", got)
	}
	if got := w.Header().Get("X-Akari-Analytics-Snapshot"); !strings.Contains(got, "state=stale") || !strings.Contains(got, "fresh=60") || !strings.Contains(got, "stale=900") {
		t.Fatalf("snapshot header = %q, want stale state and policy", got)
	}
	if got := w.Header().Get("Server-Timing"); !strings.Contains(got, "snapshot-stale") || !strings.Contains(got, "analytics-age") {
		t.Fatalf("Server-Timing = %q, want stale lookup and age metrics", got)
	}
}
