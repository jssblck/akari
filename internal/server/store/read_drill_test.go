package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// insertGradeOutcomeSignal seeds a session_signals row with a chosen grade, outcome, and
// version, standing in for a settled graded session. grade is a pointer so a test can
// store NULL (the explicit-unscored case). The row is left with signals_stale cleared
// unless the caller marks it stale afterward, matching the settle pass's post-grade state
// that the read gate keys on.
func insertGradeOutcomeSignal(t *testing.T, st *store.Store, ctx context.Context, sid int64, version int, grade *string, outcome string) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals (session_id, signals_version, outcome, outcome_confidence, grade)
		 VALUES ($1, $2, $3, 'high', $4)`,
		sid, version, outcome, grade); err != nil {
		t.Fatalf("insert grade/outcome signal for session %d: %v", sid, err)
	}
	markSignalsFresh(t, st, ctx, sid)
}

// markSignalsStaleFor sets the read gate's staleness flag, so a session with a
// current-version signals row still reads as ungraded: the letter and concrete-outcome
// filters (which require NOT s.signals_stale) drop it, and the unscored/unknown
// catch-alls pick it up, mirroring how the Insights distributions fold it.
func markSignalsStaleFor(t *testing.T, st *store.Store, ctx context.Context, sid int64) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET signals_stale = true WHERE id = $1`, sid); err != nil {
		t.Fatalf("set signals_stale for session %d: %v", sid, err)
	}
}

// idSet collects the session ids a filtered list returned, for order-independent assertion.
func idSet(rows []store.SessionRow) map[int64]bool {
	m := make(map[int64]bool, len(rows))
	for _, r := range rows {
		m[r.ID] = true
	}
	return m
}

// TestSessionFilterGradeOutcome pins the drill-through filters against the exact buckets
// the Insights distributions count. A letter grade or a concrete outcome matches only
// through a current-version, non-stale signals row; the unscored and unknown catch-alls
// match the complement, folding in the explicit NULL-grade or unknown row, the stale or
// non-current-version row, and the session with no row at all, exactly as the panel's
// LEFT JOIN + coalesce does. It also confirms CountAllSessions agrees with the list
// through the shared conds().
func TestSessionFilterGradeOutcome(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	ada := seedUser(t, st, "ada")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	grade := func(g string) *string { return &g }
	// A signals-bearing session needs a non-zero message_count so the default empty-hide
	// does not drop it; setSessionShape stamps that along with the timestamps.
	now := time.Now()
	seed := func(src string, version int, g *string, outcome string) int64 {
		sid := seedSession(t, st, ada, pid, src)
		setSessionShape(t, st, ctx, sid, now.Add(-time.Hour), now.Add(-30*time.Minute), 10, 3)
		insertGradeOutcomeSignal(t, st, ctx, sid, version, g, outcome)
		return sid
	}

	aID := seed("s-a", quality.Version, grade("A"), "completed")
	fID := seed("s-f", quality.Version, grade("F"), "errored")
	unscoredID := seed("s-unscored", quality.Version, nil, "unknown") // explicit: current row, NULL grade, unknown outcome
	abandonedID := seed("s-abandoned", quality.Version, grade("C"), "abandoned")
	staleID := seed("s-stale", quality.Version, grade("A"), "completed") // current version but flagged stale
	markSignalsStaleFor(t, st, ctx, staleID)
	oldVerID := seed("s-oldver", quality.Version+999, grade("A"), "completed") // non-current version
	// A session with no signals row at all: seed the session but no signal.
	noRowID := seedSession(t, st, ada, pid, "s-norow")
	setSessionShape(t, st, ctx, noRowID, now.Add(-time.Hour), now.Add(-30*time.Minute), 10, 3)

	list := func(f store.SessionFilter) []store.SessionRow {
		t.Helper()
		rows, err := st.ListAllSessions(ctx, f)
		if err != nil {
			t.Fatalf("list sessions: %v", err)
		}
		return rows
	}
	// countAgrees checks CountAllSessions matches the list length for the same filter, the
	// footer-vs-list invariant the shared conds() is meant to hold.
	countAgrees := func(f store.SessionFilter, want int) {
		t.Helper()
		rows := list(f)
		if len(rows) != want {
			t.Errorf("list len = %d, want %d (filter %+v)", len(rows), want, f)
		}
		total, _, err := st.CountAllSessions(ctx, f)
		if err != nil {
			t.Fatalf("count sessions: %v", err)
		}
		if total != want {
			t.Errorf("count = %d, want %d (filter %+v)", total, want, f)
		}
	}

	// Grade A: only the current, non-stale A session. The stale-flag A and the
	// non-current-version A both fold into unscored instead, matching the panel's bars.
	if got := idSet(list(store.SessionFilter{Grade: "A"})); len(got) != 1 || !got[aID] {
		t.Errorf("grade A = %v, want only session %d", got, aID)
	}
	countAgrees(store.SessionFilter{Grade: "A"}, 1)

	if got := idSet(list(store.SessionFilter{Grade: "F"})); len(got) != 1 || !got[fID] {
		t.Errorf("grade F = %v, want only session %d", got, fID)
	}

	// Unscored: the panel's catch-all, all three cases INCLUDED: the explicit NULL-grade
	// row, the stale-flag and non-current-version rows, and the session with no row.
	if got := idSet(list(store.SessionFilter{Grade: "unscored"})); len(got) != 4 ||
		!got[unscoredID] || !got[staleID] || !got[oldVerID] || !got[noRowID] {
		t.Errorf("grade unscored = %v, want exactly explicit NULL %d + stale %d + old-version %d + no-row %d",
			got, unscoredID, staleID, oldVerID, noRowID)
	}
	countAgrees(store.SessionFilter{Grade: "unscored"}, 4)

	// Outcome abandoned: only the abandoned session; the stale and old-version rows fold
	// into unknown, not into their stored outcome.
	if got := idSet(list(store.SessionFilter{Outcome: "abandoned"})); len(got) != 1 || !got[abandonedID] {
		t.Errorf("outcome abandoned = %v, want only session %d", got, abandonedID)
	}
	countAgrees(store.SessionFilter{Outcome: "abandoned"}, 1)

	if got := idSet(list(store.SessionFilter{Outcome: "completed"})); len(got) != 1 || !got[aID] {
		t.Errorf("outcome completed = %v, want only session %d (stale %d, old-version %d fold to unknown)", got, aID, staleID, oldVerID)
	}

	// Unknown: the outcome catch-all, same fold as unscored: the explicit unknown row
	// plus the stale, old-version, and no-row sessions.
	if got := idSet(list(store.SessionFilter{Outcome: "unknown"})); len(got) != 4 ||
		!got[unscoredID] || !got[staleID] || !got[oldVerID] || !got[noRowID] {
		t.Errorf("outcome unknown = %v, want explicit unknown %d + stale %d + old-version %d + no-row %d",
			got, unscoredID, staleID, oldVerID, noRowID)
	}
	countAgrees(store.SessionFilter{Outcome: "unknown"}, 4)

	// A grade with no matching current row returns nothing rather than the catch-all set.
	if got := list(store.SessionFilter{Grade: "B"}); len(got) != 0 {
		t.Errorf("grade B = %d rows, want 0", len(got))
	}
	countAgrees(store.SessionFilter{Grade: "B"}, 0)

	// Grade and outcome combine (AND): the A session is completed, so both match it.
	if got := idSet(list(store.SessionFilter{Grade: "A", Outcome: "completed"})); len(got) != 1 || !got[aID] {
		t.Errorf("grade A + completed = %v, want only session %d", got, aID)
	}
	// A grade paired with a non-matching outcome yields nothing.
	if got := list(store.SessionFilter{Grade: "A", Outcome: "errored"}); len(got) != 0 {
		t.Errorf("grade A + errored = %d rows, want 0", len(got))
	}
	// The catch-alls combine too: every unscored session here is also outcome-unknown
	// (the fold is the same missing-row set), so pairing them keeps all four.
	if got := idSet(list(store.SessionFilter{Grade: "unscored", Outcome: "unknown"})); len(got) != 4 {
		t.Errorf("unscored + unknown = %v, want the 4 catch-all sessions", got)
	}
}

// TestDrillFiltersMatchQualityDistribution is the drift guard: it seeds the same session
// shape TestQualityDistribution uses (letters across users, an explicit unscored row, a
// stale-version row, a no-row session, and every concrete outcome), then asserts every
// Grades and Outcomes bar count equals CountAllSessions for the drill-through filter that
// bar links to. If the panel's definition and the filter's ever diverge again, this fails
// on the exact bucket.
func TestDrillFiltersMatchQualityDistribution(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	ada := seedUser(t, st, "ada")
	grace := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	recent := time.Now().Add(-24 * time.Hour)
	mk := func(user int64, src string, version int, outcome, grade string) {
		sid := seedSession(t, st, user, pid, src)
		setSessionShape(t, st, ctx, sid, recent, recent.Add(10*time.Minute), 20, 2)
		insertSignal(t, st, ctx, sid, version, outcome, grade)
	}
	mk(ada, "d1", quality.Version, "completed", "A")
	mk(ada, "d2", quality.Version, "completed", "A")
	mk(ada, "d3", quality.Version, "errored", "C")
	mk(ada, "d4", quality.Version, "unknown", "")             // explicitly unscored
	mk(ada, "d6stale", quality.Version+999, "completed", "A") // stale version -> catch-all
	mk(grace, "d7", quality.Version, "abandoned", "B")
	mk(grace, "d8", quality.Version, "completed", "F")
	noneID := seedSession(t, st, ada, pid, "d9none") // no signals row at all -> catch-all
	setSessionShape(t, st, ctx, noneID, recent, recent.Add(10*time.Minute), 20, 2)

	dist, err := st.QualityDistribution(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("quality distribution: %v", err)
	}
	// Sanity-pin the fixture first, so an unexpectedly empty distribution cannot make the
	// bucket loop below pass vacuously.
	if dist.Sessions != 8 {
		t.Fatalf("Sessions = %d, want 8 seeded", dist.Sessions)
	}
	g := countByKey(dist.Grades)
	if g["A"] != 2 || g["B"] != 1 || g["C"] != 1 || g["D"] != 0 || g["F"] != 1 || g[""] != 3 {
		t.Fatalf("grade counts = %+v, want A2 B1 C1 D0 F1 unscored3", g)
	}

	// Every bar links with the panel key mapped to the filter value (the empty grade key
	// is the "unscored" sentinel, see web.GradeFilterKey). IncludeEmpty matches the
	// panel's base, which counts sessions regardless of message_count.
	count := func(f store.SessionFilter) int {
		t.Helper()
		f.IncludeEmpty = true
		total, _, err := st.CountAllSessions(ctx, f)
		if err != nil {
			t.Fatalf("count sessions (filter %+v): %v", f, err)
		}
		return total
	}
	for _, gb := range dist.Grades {
		key := gb.Key
		if key == "" {
			key = "unscored"
		}
		if got := count(store.SessionFilter{Grade: key}); got != gb.Count {
			t.Errorf("grade bucket %q: panel counts %d, drill filter matches %d", key, gb.Count, got)
		}
	}
	for _, ob := range dist.Outcomes {
		if got := count(store.SessionFilter{Outcome: ob.Key}); got != ob.Count {
			t.Errorf("outcome bucket %q: panel counts %d, drill filter matches %d", ob.Key, ob.Count, got)
		}
	}
}

// TestSessionFilterSince pins the trailing-window bound the drill-through carries: the
// list narrows to sessions active at or after the bound, and CountAllSessions agrees.
func TestSessionFilterSince(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	ada := seedUser(t, st, "ada")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	recent := seedSession(t, st, ada, pid, "recent")
	setSessionShape(t, st, ctx, recent, now.Add(-2*time.Hour), now.Add(-time.Hour), 10, 3)
	old := seedSession(t, st, ada, pid, "old")
	setSessionShape(t, st, ctx, old, now.AddDate(0, 0, -60), now.AddDate(0, 0, -60), 10, 3)
	// ListAllSessions bounds Since on s.updated_at; setSessionShape moves started/ended but
	// not updated_at, so pin updated_at directly to the same instants the window tests.
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET updated_at = $2 WHERE id = $1", recent, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET updated_at = $2 WHERE id = $1", old, now.AddDate(0, 0, -60)); err != nil {
		t.Fatal(err)
	}

	f := store.SessionFilter{Since: now.AddDate(0, 0, -30)}
	rows, err := st.ListAllSessions(ctx, f)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if got := idSet(rows); len(got) != 1 || !got[recent] {
		t.Errorf("since -30d = %v, want only recent session %d", got, recent)
	}
	total, _, err := st.CountAllSessions(ctx, f)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if total != 1 {
		t.Errorf("count since -30d = %d, want 1", total)
	}
}
