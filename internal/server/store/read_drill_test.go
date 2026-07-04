package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// insertGradeOutcomeSignal seeds a session_signals row with a chosen grade and outcome,
// standing in for a settled graded session. grade is a pointer so a test can store NULL (the
// explicit-unscored case). markSignalsFresh clears signals_stale afterward, matching the
// settle pass's post-grade state that the read gate keys on.
func insertGradeOutcomeSignal(t *testing.T, st *store.Store, ctx context.Context, sid int64, grade *string, outcome string) {
	t.Helper()
	// score and grade are a matched pair (session_signals_score_grade_ck): seed a score
	// whenever a grade is set, and leave both NULL for an ungraded (nil-grade) row.
	var score any
	if grade != nil {
		score = representativeScore(*grade)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals (session_id, outcome, outcome_confidence, score, grade)
		 VALUES ($1, $2, 'high', $3, $4)`,
		sid, outcome, score, grade); err != nil {
		t.Fatalf("insert grade/outcome signal for session %d: %v", sid, err)
	}
	markSignalsFresh(t, st, ctx, sid)
}

// representativeScore returns a score that bands to the given letter under quality.GradeFor,
// so a test seeding a graded signals row can supply a score consistent with the grade (the
// two must be set together). It is the mid-band value for each letter.
func representativeScore(grade string) int {
	switch grade {
	case "A":
		return 95
	case "B":
		return 82
	case "C":
		return 67
	case "D":
		return 50
	default: // "F"
		return 20
	}
}

// markSignalsStaleFor sets the read gate's staleness flag, so a session with a signals row
// still reads as ungraded: the letter and concrete-outcome filters (which require NOT
// s.signals_stale) drop it, and the unscored/unknown catch-alls pick it up, mirroring how the
// Insights distributions fold it.
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
// through a non-stale signals row; the unscored and unknown catch-alls match the complement,
// folding in the explicit NULL-grade or unknown row, the stale row, and the session with no
// row at all, exactly as the panel's LEFT JOIN + coalesce does. It also confirms
// CountAllSessions agrees with the list through the shared conds().
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
	seed := func(src string, g *string, outcome string) int64 {
		sid := seedSession(t, st, ada, pid, src)
		setSessionShape(t, st, ctx, sid, now.Add(-time.Hour), now.Add(-30*time.Minute), 10, 3)
		insertGradeOutcomeSignal(t, st, ctx, sid, g, outcome)
		return sid
	}

	aID := seed("s-a", grade("A"), "completed")
	fID := seed("s-f", grade("F"), "errored")
	unscoredID := seed("s-unscored", nil, "unknown") // explicit: current row, NULL grade, unknown outcome
	abandonedID := seed("s-abandoned", grade("C"), "abandoned")
	staleID := seed("s-stale", grade("A"), "completed") // graded but flagged stale
	markSignalsStaleFor(t, st, ctx, staleID)
	// A session with no signals row at all: seed the session but no signal.
	noRowID := seedSession(t, st, ada, pid, "s-norow")
	setSessionShape(t, st, ctx, noRowID, now.Add(-time.Hour), now.Add(-30*time.Minute), 10, 3)

	list := func(f store.SessionFilter) []store.SessionRow {
		t.Helper()
		rows, _, err := st.ListAllSessions(ctx, f)
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

	// Grade A: only the non-stale A session. The stale-flag A folds into unscored instead,
	// matching the panel's bars.
	if got := idSet(list(store.SessionFilter{Grade: "A"})); len(got) != 1 || !got[aID] {
		t.Errorf("grade A = %v, want only session %d", got, aID)
	}
	countAgrees(store.SessionFilter{Grade: "A"}, 1)

	if got := idSet(list(store.SessionFilter{Grade: "F"})); len(got) != 1 || !got[fID] {
		t.Errorf("grade F = %v, want only session %d", got, fID)
	}

	// Unscored: the panel's catch-all, all cases INCLUDED: the explicit NULL-grade row, the
	// stale-flag row, and the session with no row.
	if got := idSet(list(store.SessionFilter{Grade: "unscored"})); len(got) != 3 ||
		!got[unscoredID] || !got[staleID] || !got[noRowID] {
		t.Errorf("grade unscored = %v, want exactly explicit NULL %d + stale %d + no-row %d",
			got, unscoredID, staleID, noRowID)
	}
	countAgrees(store.SessionFilter{Grade: "unscored"}, 3)

	// Outcome abandoned: only the abandoned session; the stale row folds into unknown, not
	// into its stored outcome.
	if got := idSet(list(store.SessionFilter{Outcome: "abandoned"})); len(got) != 1 || !got[abandonedID] {
		t.Errorf("outcome abandoned = %v, want only session %d", got, abandonedID)
	}
	countAgrees(store.SessionFilter{Outcome: "abandoned"}, 1)

	if got := idSet(list(store.SessionFilter{Outcome: "completed"})); len(got) != 1 || !got[aID] {
		t.Errorf("outcome completed = %v, want only session %d (stale %d folds to unknown)", got, aID, staleID)
	}

	// Unknown: the outcome catch-all, same fold as unscored: the explicit unknown row plus
	// the stale and no-row sessions.
	if got := idSet(list(store.SessionFilter{Outcome: "unknown"})); len(got) != 3 ||
		!got[unscoredID] || !got[staleID] || !got[noRowID] {
		t.Errorf("outcome unknown = %v, want explicit unknown %d + stale %d + no-row %d",
			got, unscoredID, staleID, noRowID)
	}
	countAgrees(store.SessionFilter{Outcome: "unknown"}, 3)

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
	// The catch-alls combine too: every unscored session here is also outcome-unknown (the
	// fold is the same missing-row set), so pairing them keeps all three.
	if got := idSet(list(store.SessionFilter{Grade: "unscored", Outcome: "unknown"})); len(got) != 3 {
		t.Errorf("unscored + unknown = %v, want the 3 catch-all sessions", got)
	}
}

// TestDrillFiltersMatchQualityDistribution is the drift guard: it seeds the same session
// shape TestQualityDistribution uses (letters across users, an explicit unscored row, a
// stale row, a no-row session, and every concrete outcome), then asserts every Grades and
// Outcomes bar count equals CountAllSessions for the drill-through filter that bar links to.
// If the panel's definition and the filter's ever diverge again, this fails on the exact
// bucket.
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
	mk := func(user int64, src string, outcome, grade string) {
		sid := seedSession(t, st, user, pid, src)
		setSessionShape(t, st, ctx, sid, recent, recent.Add(10*time.Minute), 20, 2)
		insertSignal(t, st, ctx, sid, outcome, grade)
	}
	mk(ada, "d1", "completed", "A")
	mk(ada, "d2", "completed", "A")
	mk(ada, "d3", "errored", "C")
	mk(ada, "d4", "unknown", "") // explicitly unscored
	staleID := seedSession(t, st, ada, pid, "d6stale")
	setSessionShape(t, st, ctx, staleID, recent, recent.Add(10*time.Minute), 20, 2)
	insertSignal(t, st, ctx, staleID, "completed", "A")
	markSignalsStaleFor(t, st, ctx, staleID) // stale -> catch-all
	mk(grace, "d7", "abandoned", "B")
	mk(grace, "d8", "completed", "F")
	noneID := seedSession(t, st, ada, pid, "d9none") // no signals row at all -> catch-all
	setSessionShape(t, st, ctx, noneID, recent, recent.Add(10*time.Minute), 20, 2)
	// A zero-message graded session: the panel counts sessions regardless of
	// message_count, so it lands in the grade-B bucket, but the drill feed hides
	// empties by default. The drill link therefore carries IncludeEmpty (the count
	// closure below sets it), and this row asserts the bar count still equals the
	// drilled count once the feed shows empties. Without IncludeEmpty the drilled
	// count would fall one short of the bar.
	emptyGraded := seedSession(t, st, grace, pid, "d10empty")
	setSessionShape(t, st, ctx, emptyGraded, recent, recent.Add(10*time.Minute), 0, 0)
	insertSignal(t, st, ctx, emptyGraded, "abandoned", "B")

	dist, err := st.QualityDistribution(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("quality distribution: %v", err)
	}
	// Sanity-pin the fixture first, so an unexpectedly empty distribution cannot make the
	// bucket loop below pass vacuously.
	if dist.Sessions != 9 {
		t.Fatalf("Sessions = %d, want 9 seeded", dist.Sessions)
	}
	g := countByKey(dist.Grades)
	// The zero-message session grades B, so B is now 2: the panel counts it despite its
	// empty message_count, which is exactly why the drill must carry IncludeEmpty.
	if g["A"] != 2 || g["B"] != 2 || g["C"] != 1 || g["D"] != 0 || g["F"] != 1 || g[""] != 3 {
		t.Fatalf("grade counts = %+v, want A2 B2 C1 D0 F1 unscored3", g)
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
// list narrows to sessions STARTED at or after the bound, matching the column the
// Insights panels window by (s.started_at), and CountAllSessions agrees.
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
	// The bound is on s.started_at, which setSessionShape sets, so the started times above
	// are what the window tests. No updated_at fiddling is needed.

	f := store.SessionFilter{Since: now.AddDate(0, 0, -30)}
	rows, _, err := st.ListAllSessions(ctx, f)
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

// TestSessionFilterSinceWindowsStartedAt is the projection-consistency counterexample:
// the drill feed windows by s.started_at, the same column the Insights panels bucket by,
// NOT s.updated_at. A session started before the window but bumped (updated) inside it by
// a late reparse stays OUT of the feed, matching the panel bucket that never counted it;
// the inverse, started inside the window but updated after, stays IN, matching a panel
// that did count it. An updated_at bound would flip both, so the feed and the panel would
// disagree on exactly these two rows.
func TestSessionFilterSinceWindowsStartedAt(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	ada := seedUser(t, st, "ada")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	windowStart := now.AddDate(0, 0, -30)

	// startedBeforeUpdatedInside: started 60 days ago (before the window) but updated
	// yesterday (inside it). An updated_at bound would wrongly include it; a started_at
	// bound excludes it, matching the panel bucket that windows on started_at.
	before := seedSession(t, st, ada, pid, "started-before")
	setSessionShape(t, st, ctx, before, now.AddDate(0, 0, -60), now.AddDate(0, 0, -60), 10, 3)
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET updated_at = $2 WHERE id = $1", before, now.AddDate(0, 0, -1)); err != nil {
		t.Fatal(err)
	}
	// startedInsideUpdatedAfter: started 10 days ago (inside the window) but its
	// updated_at sits before the window (an odd but legal state); a started_at bound
	// includes it, matching the panel that counted it by its start.
	inside := seedSession(t, st, ada, pid, "started-inside")
	setSessionShape(t, st, ctx, inside, now.AddDate(0, 0, -10), now.AddDate(0, 0, -10), 10, 3)
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET updated_at = $2 WHERE id = $1", inside, now.AddDate(0, 0, -45)); err != nil {
		t.Fatal(err)
	}

	f := store.SessionFilter{Since: windowStart}
	rows, _, err := st.ListAllSessions(ctx, f)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := idSet(rows)
	if got[before] {
		t.Errorf("session started before the window but updated inside it must be excluded (started_at bound), got included")
	}
	if !got[inside] {
		t.Errorf("session started inside the window must be included even if updated before it (started_at bound), got excluded")
	}
}
