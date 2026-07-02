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

// TestPublicAndAuthedProjectAnalyticsReconcile pins the one intentional gap between
// the two project usage surfaces. The signed-in project page reads Analytics with no
// upper bound (so its panel reconciles with its unbounded session table), while the
// public /p/<id> page bounds the panel to the end of today (so its headline reconciles
// with the trailing-year heatmap, which stops at today). The two therefore agree for
// every real, past-dated usage event and differ only by a future-dated one, which is a
// malformed-transcript case that does not occur in practice. This test seeds exactly
// that boundary: a past event both surfaces count, and a future event only the authed
// surface counts, so the sole divergence is the future event and nothing else.
func TestPublicAndAuthedProjectAnalyticsReconcile(t *testing.T) {
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
	seedUsage(t, st, sid, "claude-opus-4-8", 1.0, 100, 50, 1, "past")    // yesterday, in-window for both
	seedUsage(t, st, sid, "claude-opus-4-8", 9.0, 900, 90, -3, "future") // 3 days ahead, authed-only

	since := time.Now().Add(-365 * 24 * time.Hour)
	// The two filters the handlers build (minus the authed page's user/agent/machine
	// narrowing, absent on a bare project load): the authed panel is unbounded above; the
	// public panel bounds to the end of today and omits the by-user split it never renders.
	authed, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: projectID, Since: since})
	if err != nil {
		t.Fatal(err)
	}
	public, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: projectID, Since: since, Until: endOfTodayUTC(), OmitUsers: true})
	if err != nil {
		t.Fatal(err)
	}

	// The authed surface folds in the future event; the public surface excludes it.
	if authed.TotalIn != 1000 {
		t.Fatalf("authed TotalIn = %d, want 1000 (past + future)", authed.TotalIn)
	}
	if public.TotalIn != 100 {
		t.Fatalf("public TotalIn = %d, want 100 (future excluded)", public.TotalIn)
	}
	// The gap is exactly the future event and nothing else: bounding the authed filter
	// the same way the public one is bound makes the two headlines identical, proving the
	// only difference between the surfaces is the Until bound (not OmitUsers, which sits
	// outside the headline the by-agent split sums).
	authedBounded, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: projectID, Since: since, Until: endOfTodayUTC()})
	if err != nil {
		t.Fatal(err)
	}
	if authedBounded.TotalIn != public.TotalIn || authedBounded.TotalOut != public.TotalOut ||
		!costsEqual(authedBounded.TotalCost, public.TotalCost) || authedBounded.Sessions != public.Sessions {
		t.Fatalf("bounded authed and public disagree beyond the by-user split: authed=%+v public=%+v", authedBounded, public)
	}
	// The public read omits the by-user split; the authed read carries it. That is the
	// only other difference, and it never touches the reconciled headline above.
	if public.Users != nil {
		t.Errorf("public Analytics should omit the by-user split, got %+v", public.Users)
	}
	if len(authed.Users) == 0 {
		t.Error("authed Analytics should carry the by-user split")
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
