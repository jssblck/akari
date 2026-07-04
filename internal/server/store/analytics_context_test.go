package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// insertContextSignal writes a session_signals row with the context-health columns set, so
// a cohort test can pin the aggregate without driving the whole gather path (signals_context_test
// already covers that). peak and resets are pointers so a test can store NULL (a session
// with no usage), which the aggregate must exclude from its measured cohort. fresh controls
// whether signals_stale is cleared afterward, standing in for a session whose grade the fleet
// read gate should see (true) versus one whose projection moved since it was graded (false).
func insertContextSignal(t *testing.T, st *store.Store, ctx context.Context, sid int64, fresh bool, peak *int64, resets *int) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals
		   (session_id, outcome, outcome_confidence,
		    peak_context_tokens, context_reset_count)
		 VALUES ($1, 'completed', 'high', $2, $3)`,
		sid, peak, resets); err != nil {
		t.Fatalf("insert context signal for session %d: %v", sid, err)
	}
	if fresh {
		markSignalsFresh(t, st, ctx, sid)
	}
}

// TestContextHealth pins the cohort aggregate: the peak percentiles read actual stored
// peaks, the reset figures sum the per-session counts, and only fresh rows with a measured
// (non-null) peak are in the cohort. It also honors the window and per-user scoping.
func TestContextHealth(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	ada := seedUser(t, st, "ada")
	grace := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	recent := time.Now().Add(-1 * time.Hour)
	old := time.Now().Add(-400 * 24 * time.Hour)

	ptr := func(v int64) *int64 { return &v }
	rst := func(v int) *int { return &v }
	seed := func(user int64, src string, started time.Time, fresh bool, peak *int64, resets *int) {
		sid := seedSession(t, st, user, pid, src)
		setSessionShape(t, st, ctx, sid, started, started.Add(10*time.Minute), 20, 5)
		insertContextSignal(t, st, ctx, sid, fresh, peak, resets)
	}

	seed(ada, "c1", recent, true, ptr(100000), rst(0))       // in window, fresh
	seed(ada, "c2", recent, true, ptr(200000), rst(2))       // in window, fresh
	seed(grace, "c3", recent, true, ptr(300000), rst(1))     // in window, fresh, other user
	seed(grace, "c4", old, true, ptr(400000), rst(3))        // out of window, fresh
	seed(ada, "c5stale", recent, false, ptr(999999), rst(9)) // graded but the projection moved since -> excluded
	seed(ada, "c6null", recent, true, nil, nil)              // no measured context -> excluded

	// Unscoped: the four fresh measured rows (c1..c4). percentile_disc returns a
	// real stored peak: for [100k,200k,300k,400k] the median is 200k and the p90 is 400k.
	all, err := st.ContextHealth(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("context health (all): %v", err)
	}
	if !all.HasData() {
		t.Fatal("unscoped context health should have data")
	}
	if all.Sessions != 4 || all.PeakTokensP50 != 200000 || all.PeakTokensP90 != 400000 || all.PeakTokensMax != 400000 ||
		all.TotalResets != 6 || all.SessionsWithReset != 3 {
		t.Errorf("unscoped = %+v, want {Sessions 4, P50 200000, P90 400000, Max 400000, TotalResets 6, SessionsWithReset 3}", all)
	}

	// Windowed: a trailing window drops the old session (c4). Peaks [100k,200k,300k].
	windowed, err := st.ContextHealth(ctx, store.AnalyticsFilter{Since: time.Now().Add(-90 * 24 * time.Hour)})
	if err != nil {
		t.Fatalf("context health (windowed): %v", err)
	}
	if windowed.Sessions != 3 || windowed.PeakTokensP50 != 200000 || windowed.PeakTokensP90 != 300000 || windowed.PeakTokensMax != 300000 ||
		windowed.TotalResets != 3 || windowed.SessionsWithReset != 2 {
		t.Errorf("windowed = %+v, want {Sessions 3, P50 200000, P90 300000, Max 300000, TotalResets 3, SessionsWithReset 2}", windowed)
	}

	// Ada only: her two fresh measured rows (c1, c2); the stale c5 and null-peak c6 stay out.
	adaOnly, err := st.ContextHealth(ctx, store.AnalyticsFilter{Username: "ada"})
	if err != nil {
		t.Fatalf("context health (ada): %v", err)
	}
	if adaOnly.Sessions != 2 || adaOnly.PeakTokensP50 != 100000 || adaOnly.PeakTokensP90 != 200000 || adaOnly.PeakTokensMax != 200000 ||
		adaOnly.TotalResets != 2 || adaOnly.SessionsWithReset != 1 {
		t.Errorf("ada = %+v, want {Sessions 2, P50 100000, P90 200000, Max 200000, TotalResets 2, SessionsWithReset 1}", adaOnly)
	}

	// Until on a session-derived aggregate. The upper bound is compared against s.started_at,
	// not usage_events.occurred_at (this cohort carries no usage_events to date), so a bound
	// set 200 days back keeps only the old session (c4 at -400d) and drops the recent ones.
	// A misrouted bound that named ue.occurred_at here would fail to compile the query at all,
	// so this pins the clauseFor Until branch to the same time column its Since branch uses.
	untilOld, err := st.ContextHealth(ctx, store.AnalyticsFilter{Until: time.Now().Add(-200 * 24 * time.Hour)})
	if err != nil {
		t.Fatalf("context health (until): %v", err)
	}
	if untilOld.Sessions != 1 || untilOld.PeakTokensP50 != 400000 || untilOld.PeakTokensP90 != 400000 || untilOld.PeakTokensMax != 400000 ||
		untilOld.TotalResets != 3 || untilOld.SessionsWithReset != 1 {
		t.Errorf("until = %+v, want {Sessions 1, P50 400000, P90 400000, Max 400000, TotalResets 3, SessionsWithReset 1}", untilOld)
	}
}

// TestContextHealthEmpty confirms a scope with no measured session reports no data, so the
// panel shows a note rather than a row of zeroes.
func TestContextHealthEmpty(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	h, err := st.ContextHealth(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("context health (empty): %v", err)
	}
	if h.HasData() {
		t.Errorf("empty scope should have no data, got %+v", h)
	}
}
