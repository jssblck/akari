package store_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// insertSignal writes a session_signals row directly, so a distribution test can pin a
// session's grade and outcome without driving the whole computation (which signals_test
// already covers). A scored row gets an arbitrary valid score; an empty grade is the
// unscored bucket (NULL grade and score).
func insertSignal(t *testing.T, st *store.Store, ctx context.Context, sid int64, version int, outcome, grade string) {
	t.Helper()
	var score, gradeArg any
	if grade != "" {
		score, gradeArg = 80, grade
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals (session_id, signals_version, outcome, outcome_confidence, score, grade)
		 VALUES ($1, $2, $3, 'high', $4, $5)`, sid, version, outcome, score, gradeArg); err != nil {
		t.Fatalf("insert signal for session %d: %v", sid, err)
	}
}

// setSessionShape stamps the facts the archetype banding and the windowing read: the
// start/end span, the message and user-message counts.
func setSessionShape(t *testing.T, st *store.Store, ctx context.Context, sid int64, started, ended time.Time, messages, userMessages int) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx,
		`UPDATE sessions SET started_at = $2, ended_at = $3, message_count = $4, user_message_count = $5 WHERE id = $1`,
		sid, started, ended, messages, userMessages); err != nil {
		t.Fatalf("set session shape for %d: %v", sid, err)
	}
}

// countByKey indexes a canonical-ordered distribution by key for assertion.
func countByKey(rows []store.LabeledCount) map[string]int {
	m := map[string]int{}
	for _, r := range rows {
		m[r.Key] = r.Count
	}
	return m
}

// TestQualityDistribution confirms the grade and outcome splits fold into the canonical
// order with zero-filled buckets, count only current-version rows, and respect the
// analytics scoping (window and user).
func TestQualityDistribution(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	ua := seedUser(t, st, "ada")
	ub := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	recent := time.Now().Add(-24 * time.Hour)
	old := time.Now().Add(-400 * 24 * time.Hour)

	mk := func(user int64, src string, started time.Time, version int, outcome, grade string) int64 {
		sid := seedSession(t, st, user, pid, src)
		setSessionShape(t, st, ctx, sid, started, started.Add(10*time.Minute), 20, 2)
		insertSignal(t, st, ctx, sid, version, outcome, grade)
		return sid
	}
	// Ada: current-version A, A, C, and an explicit unscored row, all in window; a fifth
	// in-window session with NO signals row and a sixth with a STALE-version row, both of
	// which must fold into the unscored/unknown bucket (the missing-row path) rather than
	// drop out; plus an old F outside a 90-day window.
	mk(ua, "a1", recent, quality.Version, "completed", "A")
	mk(ua, "a2", recent, quality.Version, "completed", "A")
	mk(ua, "a3", recent, quality.Version, "errored", "C")
	mk(ua, "a4", recent, quality.Version, "unknown", "")            // explicitly unscored
	mk(ua, "a6stale", recent, quality.Version+999, "completed", "A") // stale version -> missing bucket
	mk(ua, "a5old", old, quality.Version, "completed", "F")          // outside the window
	// A session with no signals row at all (mid-parse, pre-backfill): still counted.
	noneID := seedSession(t, st, ua, pid, "a7none")
	setSessionShape(t, st, ctx, noneID, recent, recent.Add(10*time.Minute), 20, 2)
	mk(ub, "b1", recent, quality.Version, "abandoned", "B") // other user

	// Window: last 90 days, all users. The old F drops out; the stale-version and the
	// no-row sessions stay, counted as unscored/unknown.
	since := time.Now().Add(-90 * 24 * time.Hour)
	dist, err := st.QualityDistribution(ctx, store.AnalyticsFilter{Since: since})
	if err != nil {
		t.Fatalf("quality distribution: %v", err)
	}
	if dist.Sessions != 7 { // a1,a2,a3,a4,a6stale,a7none (Ada) + b1 (Grace); old excluded
		t.Errorf("Sessions = %d, want 7", dist.Sessions)
	}
	// Canonical grade order with zero-fill.
	wantGradeOrder := []string{"A", "B", "C", "D", "F", ""}
	for i, g := range dist.Grades {
		if g.Key != wantGradeOrder[i] {
			t.Fatalf("grade order[%d] = %q, want %q", i, g.Key, wantGradeOrder[i])
		}
	}
	g := countByKey(dist.Grades)
	// unscored = a4 (explicit) + a6stale (stale row) + a7none (no row).
	if g["A"] != 2 || g["B"] != 1 || g["C"] != 1 || g["D"] != 0 || g["F"] != 0 || g[""] != 3 {
		t.Errorf("grade counts = %+v, want A2 B1 C1 D0 F0 unscored3", g)
	}
	o := countByKey(dist.Outcomes)
	if o["completed"] != 2 || o["errored"] != 1 || o["abandoned"] != 1 || o["unknown"] != 3 {
		t.Errorf("outcome counts = %+v, want completed2 errored1 abandoned1 unknown3", o)
	}

	// Scope to Ada only: Grace's abandoned B drops out.
	adaOnly, err := st.QualityDistribution(ctx, store.AnalyticsFilter{Since: since, Username: "ada"})
	if err != nil {
		t.Fatalf("ada distribution: %v", err)
	}
	if adaOnly.Sessions != 6 {
		t.Errorf("ada Sessions = %d, want 6", adaOnly.Sessions)
	}
	if countByKey(adaOnly.Outcomes)["abandoned"] != 0 {
		t.Errorf("ada should have no abandoned session, got %d", countByKey(adaOnly.Outcomes)["abandoned"])
	}
}

// TestArchetypeDistribution confirms each session bands into the archetype its facts
// imply, matching quality.ClassifyArchetype, and that the result folds into the fixed
// lightest-to-heaviest order.
func TestArchetypeDistribution(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u := seedUser(t, st, "anna")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-2 * time.Hour)
	mk := func(src string, durMin, messages, userMessages int) {
		sid := seedSession(t, st, u, pid, src)
		setSessionShape(t, st, ctx, sid, base, base.Add(time.Duration(durMin)*time.Minute), messages, userMessages)
	}
	mk("quick", 1, 4, 1)         // quick
	mk("standard-msgs", 2, 20, 2) // standard by message count
	mk("deep-msgs", 2, 80, 3)     // deep by message count
	mk("marathon-dur", 180, 30, 4) // marathon by duration
	mk("automation", 300, 500, 0)  // automation (no human turn) wins over heaviness

	dist, err := st.ArchetypeDistribution(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("archetype distribution: %v", err)
	}
	wantOrder := []string{"quick", "standard", "deep", "marathon", "automation"}
	for i, a := range dist {
		if a.Key != wantOrder[i] {
			t.Fatalf("archetype order[%d] = %q, want %q", i, a.Key, wantOrder[i])
		}
	}
	c := countByKey(dist)
	if c["quick"] != 1 || c["standard"] != 1 || c["deep"] != 1 || c["marathon"] != 1 || c["automation"] != 1 {
		t.Errorf("archetype counts = %+v, want one of each", c)
	}
}

// TestArchetypeDistributionBoundaries pins the SQL banding at its exact thresholds (the
// CASE uses >=, matching quality.ClassifyArchetype) and the NULL-duration path (a
// session with no start/end span bands on its message count, the duration coalesced to
// zero), so the SQL and the Go reference cannot drift apart at the edges.
func TestArchetypeDistributionBoundaries(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u := seedUser(t, st, "katherine")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-3 * time.Hour)

	// Exactly at the standard duration threshold bands standard (>=).
	s1 := seedSession(t, st, u, pid, "at-standard")
	setSessionShape(t, st, ctx, s1, base, base.Add(quality.StandardMinutes*time.Minute), 1, 1)
	// Just under it bands quick.
	s2 := seedSession(t, st, u, pid, "under-standard")
	setSessionShape(t, st, ctx, s2, base, base.Add((quality.StandardMinutes-1)*time.Minute), 1, 1)
	// Exactly at the deep message threshold bands deep, on message count alone.
	s3 := seedSession(t, st, u, pid, "at-deep-msgs")
	setSessionShape(t, st, ctx, s3, base, base.Add(time.Minute), quality.DeepMessages, 1)
	// No start/end span: duration is NULL, coalesced to zero, so it bands on messages
	// (1, well under standard) as quick rather than erroring on the NULL arithmetic.
	s4 := seedSession(t, st, u, pid, "no-span")
	if _, err := st.Pool.Exec(ctx,
		`UPDATE sessions SET message_count = 1, user_message_count = 1 WHERE id = $1`, s4); err != nil {
		t.Fatalf("set no-span shape: %v", err)
	}

	dist, err := st.ArchetypeDistribution(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("archetype distribution: %v", err)
	}
	c := countByKey(dist)
	if c["quick"] != 2 || c["standard"] != 1 || c["deep"] != 1 || c["marathon"] != 0 || c["automation"] != 0 {
		t.Errorf("boundary counts = %+v, want quick2 standard1 deep1 marathon0 automation0", c)
	}
}

// TestConcurrencyStats builds four overlapping spans with a known three-way peak and
// pins the sweep-line result: the fleet peak and when it is reached, the busiest single
// user's own peak, and the average concurrency over the covered span. It also confirms
// the per-user scoping narrows the sweep.
func TestConcurrencyStats(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	ada := seedUser(t, st, "ada")
	grace := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	at := func(min int) time.Time { return day.Add(time.Duration(min) * time.Minute) }
	span := func(user int64, src string, startMin, endMin int) {
		sid := seedSession(t, st, user, pid, src)
		setSessionShape(t, st, ctx, sid, at(startMin), at(endMin), 10, 2)
	}
	span(ada, "sA", 0, 30)    // 10:00-10:30
	span(ada, "sB", 10, 40)   // 10:10-10:40, overlaps sA -> ada runs two at once
	span(grace, "sC", 20, 50) // 10:20-10:50, three overlap in 10:20-10:30 -> fleet peak 3
	span(ada, "sD", 60, 70)   // 11:00-11:10, no overlap

	cs, err := st.ConcurrencyStats(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("concurrency stats: %v", err)
	}
	if cs.Sessions != 4 {
		t.Errorf("Sessions = %d, want 4", cs.Sessions)
	}
	if cs.FleetPeak != 3 {
		t.Errorf("FleetPeak = %d, want 3", cs.FleetPeak)
	}
	if !cs.FleetPeakAt.Equal(at(20)) {
		t.Errorf("FleetPeakAt = %s, want %s (the third session's start)", cs.FleetPeakAt, at(20))
	}
	if cs.BusiestUser != "ada" || cs.BusiestUserPeak != 2 {
		t.Errorf("busiest = (%s, %d), want (ada, 2)", cs.BusiestUser, cs.BusiestUserPeak)
	}
	// Active session-time = 30+30+30+10 = 100 min; covered span = 70 min (10:00..11:10).
	wantAvg := 100.0 / 70.0
	if math.Abs(cs.AvgConcurrent-wantAvg) > 0.01 {
		t.Errorf("AvgConcurrent = %.3f, want ~%.3f", cs.AvgConcurrent, wantAvg)
	}

	// Scope to Ada: Grace's sC drops out, so the fleet peak is Ada's own two-at-once.
	adaOnly, err := st.ConcurrencyStats(ctx, store.AnalyticsFilter{Username: "ada"})
	if err != nil {
		t.Fatalf("ada concurrency: %v", err)
	}
	if adaOnly.Sessions != 3 || adaOnly.FleetPeak != 2 || adaOnly.BusiestUser != "ada" {
		t.Errorf("ada scope = {sessions %d, peak %d, busiest %s}, want {3, 2, ada}", adaOnly.Sessions, adaOnly.FleetPeak, adaOnly.BusiestUser)
	}
}
