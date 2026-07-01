package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// endOfTodayUTC is the exclusive upper bound the OG card uses: the start of
// tomorrow (UTC), so every event through the end of today is in-window and any
// future-dated event is excluded, matching the card's heatmap which draws through
// today.
func endOfTodayUTC() time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
}

// TestAnalyticsUntilExcludesFuture guards the card's window reconciliation: a
// future-dated usage event must not inflate the total (which the card shows next to
// a heatmap that stops at today). With no upper bound the future event counts; with
// Until set to the end of today it is excluded, and the day series then sums to the
// headline total.
func TestAnalyticsUntilExcludesFuture(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	sid := seedSessionWithStats(t, st, u.ID, projectID, "claude", "sess-1", 1.0, 100, 50)
	seedUsage(t, st, sid, "claude-opus-4-8", 1.0, 100, 50, 1, "past")    // yesterday, in-window
	seedUsage(t, st, sid, "claude-opus-4-8", 9.0, 900, 90, -3, "future") // 3 days ahead

	since := time.Now().Add(-365 * 24 * time.Hour)

	// Unbounded: the future event is folded into the total.
	unbounded, err := st.Analytics(ctx, store.AnalyticsFilter{Since: since, UserIDs: []int64{u.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if unbounded.TotalIn != 1000 {
		t.Fatalf("unbounded TotalIn = %d, want 1000 (both events)", unbounded.TotalIn)
	}

	// Bounded to the end of today: the future event drops out of the headline.
	bounded, err := st.Analytics(ctx, store.AnalyticsFilter{Since: since, Until: endOfTodayUTC(), UserIDs: []int64{u.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if bounded.TotalIn != 100 {
		t.Fatalf("bounded TotalIn = %d, want 100 (future event excluded)", bounded.TotalIn)
	}

	// The visible day series now reconciles with the headline: summing the days the
	// card would draw equals the total it prints.
	var seriesIn int64
	for _, p := range bounded.Series {
		seriesIn += p.Input
	}
	if seriesIn != bounded.TotalIn {
		t.Fatalf("series input sum %d != headline TotalIn %d", seriesIn, bounded.TotalIn)
	}
}

// TestAnalyticsSnapshotSkipsDuringReparse guards the card render's reparse
// coordination at the store layer: while the reparse advisory lock is held, the
// snapshot reports not-ok (so Generate skips) rather than reading a projection that
// may be mid-rebuild; once the lock clears, it returns the analytics.
func TestAnalyticsSnapshotSkipsDuringReparse(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	sid := seedSessionWithStats(t, st, u.ID, projectID, "claude", "sess-1", 1.0, 100, 50)
	seedUsage(t, st, sid, "claude-opus-4-8", 1.0, 100, 50, 1, "u1")

	filter := store.AnalyticsFilter{Since: time.Now().Add(-365 * 24 * time.Hour), UserIDs: []int64{u.ID}}

	// Hold the reparse advisory lock: the snapshot must decline.
	lock, ok, err := st.AcquireReparseLock(ctx)
	if err != nil || !ok {
		t.Fatalf("acquire reparse lock: ok=%v err=%v", ok, err)
	}
	if _, ok, err := st.AnalyticsSnapshot(ctx, filter); err != nil || ok {
		lock.Release(ctx)
		t.Fatalf("snapshot during reparse: ok=%v err=%v, want ok=false", ok, err)
	}
	lock.Release(ctx)

	// With the lock clear, the snapshot returns the analytics.
	a, ok, err := st.AnalyticsSnapshot(ctx, filter)
	if err != nil || !ok {
		t.Fatalf("snapshot after reparse: ok=%v err=%v, want ok=true", ok, err)
	}
	if a.TotalIn != 100 {
		t.Fatalf("snapshot TotalIn = %d, want 100", a.TotalIn)
	}
}
