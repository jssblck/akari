package store_test

import (
	"context"
	"errors"
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

// TestAnalyticsOmitUsersOnlyChangesBreakdown pins the shared project snapshot's
// shape. Its end-of-today bound excludes malformed future usage, while retaining the
// by-user split lets the authenticated view reuse the same generation. Omitting that
// split for another caller may change only Users, never the headline.
func TestAnalyticsOmitUsersOnlyChangesBreakdown(t *testing.T) {
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
	shared, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: projectID, Since: since, Until: endOfTodayUTC()})
	if err != nil {
		t.Fatal(err)
	}
	omitted, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: projectID, Since: since, Until: endOfTodayUTC(), OmitUsers: true})
	if err != nil {
		t.Fatal(err)
	}
	if shared.TotalIn != 100 || omitted.TotalIn != 100 {
		t.Fatalf("bounded totals = (shared %d, omitted %d), want (100, 100)", shared.TotalIn, omitted.TotalIn)
	}
	if shared.TotalOut != omitted.TotalOut || !costsEqual(shared.TotalCost, omitted.TotalCost) || shared.Sessions != omitted.Sessions {
		t.Fatalf("OmitUsers changed the headline: shared=%+v omitted=%+v", shared, omitted)
	}
	if omitted.Users != nil {
		t.Errorf("OmitUsers left a by-user split: %+v", omitted.Users)
	}
	if len(shared.Users) == 0 {
		t.Error("shared snapshot should carry the authenticated view's by-user split")
	}
}

// TestAnalyticsSnapshotSkipsDuringRebuild guards the card render's epoch coordination
// at the store layer: while any session sits behind the store's running parser epoch,
// the snapshot reports not-ok (so Generate skips) rather than reading a projection that
// is a half-rebuilt mix of old and new parses; once every session is back at the
// running epoch, it returns the analytics.
func TestAnalyticsSnapshotSkipsDuringRebuild(t *testing.T) {
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

	// A session freshly seeded by direct SQL has no session_raw row, so it never trips
	// the epoch check; announce one and mark it stale to stand in for a fleet rebuild
	// still draining. The store is gated on testEpoch so rebuildWith's stub reducer
	// (which always rebuilds at testEpoch) is what brings the corpus current below.
	st.SetParserEpoch(testEpoch)
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-1-raw", ProjectID: projectID, Machine: "box",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if _, err := st.MarkEpochStale(ctx, ""); err != nil {
		t.Fatalf("mark epoch stale: %v", err)
	}

	// While the announced session sits behind the running epoch, the snapshot declines.
	if _, ok, err := st.AnalyticsSnapshot(ctx, filter); err != nil || ok {
		t.Fatalf("snapshot mid-rebuild: ok=%v err=%v, want ok=false", ok, err)
	}

	// Rebuild it up to the running epoch: the corpus is single-epoch again.
	rebuildWith(t, st, ann.SessionID, store.ProjectionDelta{})

	// With every session at the running epoch, the snapshot returns the analytics.
	a, ok, err := st.AnalyticsSnapshot(ctx, filter)
	if err != nil || !ok {
		t.Fatalf("snapshot after rebuild: ok=%v err=%v, want ok=true", ok, err)
	}
	if a.TotalIn != 100 {
		t.Fatalf("snapshot TotalIn = %d, want 100", a.TotalIn)
	}

	// A session that FAILED its rebuild at the running epoch must not wedge the
	// gate: its projection is permanently behind (the drain cannot advance it),
	// so the snapshot serves rather than blanking the cards forever. An epoch
	// bump (a different running epoch) gates again: the failed session gets a
	// fresh attempt there.
	if _, err := st.AppendChunk(ctx, ann.SessionID, 0, []byte("bad bytes\n")); err != nil {
		t.Fatal(err)
	}
	rerr := errors.New("malformed transcript")
	if err := st.RebuildSession(ctx, ann.SessionID, testEpoch, failingReducer{rerr}); !errors.Is(err, rerr) {
		t.Fatalf("failing rebuild returned %v, want the reducer's error", err)
	}
	if _, err := st.Pool.Exec(ctx,
		"UPDATE session_raw SET parser_epoch = $1 WHERE session_id = $2", testEpoch-1, ann.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.AnalyticsSnapshot(ctx, filter); err != nil || !ok {
		t.Fatalf("snapshot with a failed-at-current-epoch session: ok=%v err=%v, want ok=true (gate must not wedge)", ok, err)
	}

	// New bytes un-pin the failure: the worker now has a concrete rebuild path at
	// the running epoch (the appended tail might parse), so the session is due
	// again and the gate must agree and decline rather than declaring the corpus
	// current while that rebuild is pending.
	if _, err := st.AppendChunk(ctx, ann.SessionID, int64(len("bad bytes\n")), []byte("more bytes\n")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := st.AnalyticsSnapshot(ctx, filter); err != nil || ok {
		t.Fatalf("snapshot after appending to the failed session: ok=%v err=%v, want ok=false (a current-epoch rebuild is pending)", ok, err)
	}
}
