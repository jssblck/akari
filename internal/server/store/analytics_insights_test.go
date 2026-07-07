package store_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// insertSignal writes a session_signals row directly, so a distribution test can pin a
// session's grade and outcome without driving the whole computation (which signals_test
// already covers). A scored row gets an arbitrary valid score; an empty grade is the
// unscored bucket (NULL grade and score).
func insertSignal(t *testing.T, st *store.Store, ctx context.Context, sid int64, outcome, grade string) {
	t.Helper()
	// score and grade must agree (a set grade equals GradeFor(score); see migration 0040), so
	// derive a band-consistent score from the grade rather than a fixed 80 that would contradict
	// any non-B letter.
	var score, gradeArg any
	if grade != "" {
		score, gradeArg = representativeScore(grade), grade
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals (session_id, outcome, outcome_confidence, score, grade)
		 VALUES ($1, $2, 'high', $3, $4)`, sid, outcome, score, gradeArg); err != nil {
		t.Fatalf("insert signal for session %d: %v", sid, err)
	}
	markSignalsFresh(t, st, ctx, sid)
}

// insertStaleSignal writes a session_signals row exactly like insertSignal, but leaves
// signals_stale set. It stands in for a session that was graded and then had its
// projection move (a rebuild landed after the grade), which the fleet read must still
// treat as ungraded until the next settle pass regrades it.
func insertStaleSignal(t *testing.T, st *store.Store, ctx context.Context, sid int64, outcome, grade string) {
	t.Helper()
	insertSignal(t, st, ctx, sid, outcome, grade)
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET signals_stale = true WHERE id = $1`, sid); err != nil {
		t.Fatalf("set signals_stale for session %d: %v", sid, err)
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
// order with zero-filled buckets, count only fresh (non-stale) signals rows, and respect
// the analytics scoping (window and user).
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

	mk := func(user int64, src string, started time.Time, outcome, grade string) int64 {
		sid := seedSession(t, st, user, pid, src)
		setSessionShape(t, st, ctx, sid, started, started.Add(10*time.Minute), 20, 2)
		insertSignal(t, st, ctx, sid, outcome, grade)
		return sid
	}
	// Ada: A, A, C, and an explicit unscored row, all in window; a fifth in-window session
	// with NO signals row and a sixth whose signals row is STALE (graded, but the
	// projection moved since), both of which must fold into the unscored/unknown bucket
	// (the missing-row path) rather than drop out; plus an old F outside a 90-day window.
	mk(ua, "a1", recent, "completed", "A")
	mk(ua, "a2", recent, "completed", "A")
	mk(ua, "a3", recent, "errored", "C")
	mk(ua, "a4", recent, "unknown", "") // explicitly unscored
	stale := seedSession(t, st, ua, pid, "a6stale")
	setSessionShape(t, st, ctx, stale, recent, recent.Add(10*time.Minute), 20, 2)
	insertStaleSignal(t, st, ctx, stale, "completed", "A") // graded but the projection moved since -> missing bucket
	mk(ua, "a5old", old, "completed", "F")                 // outside the window
	// A session with no signals row at all (mid-parse, pre-backfill): still counted.
	noneID := seedSession(t, st, ua, pid, "a7none")
	setSessionShape(t, st, ctx, noneID, recent, recent.Add(10*time.Minute), 20, 2)
	mk(ub, "b1", recent, "abandoned", "B") // other user

	// Window: last 90 days, all users. The old F drops out; the stale and the
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

// TestQualityDrilldownWindowsOnStartedAt pins that a windowed quality bar and its drill-down
// list describe the identical session set. The bar counts sessions by when they started
// (QualityDistribution over AnalyticsFilter.Since on s.started_at); the feed reaches that list
// through ListAllSessions, which binds SessionFilter.Since to that same s.started_at column
// (conds("s.started_at")). The footgun this guards is the two windowing on different columns: a
// session started before the window but re-activated inside it (last_active_at in range,
// started_at out) would list under a last-active bound while the bar never counted it, so the
// bar and its destination would disagree. With both bounding started_at they agree, and the
// early-started session is excluded from both.
func TestQualityDrilldownWindowsOnStartedAt(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	ada := seedUser(t, st, "ada")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	inWindow := now.Add(-3 * 24 * time.Hour)      // started inside a 7-day window
	beforeWindow := now.Add(-30 * 24 * time.Hour) // started well before it
	activeNow := now.Add(-1 * time.Hour)          // last-active recently, inside the window

	// A session started inside the window, graded and current: the bar counts it and the feed
	// lists it.
	inside := seedSession(t, st, ada, pid, "started-inside")
	setSessionShape(t, st, ctx, inside, inWindow, inWindow.Add(10*time.Minute), 20, 2)
	insertSignal(t, st, ctx, inside, "completed", "A")

	// A session started BEFORE the window but re-activated inside it: its last event
	// (ended_at, and thus the generated last_active_at) lands in range while started_at does
	// not. A feed bound on last-active would surface it; ListAllSessions, which binds Since to
	// started_at, must not, matching the bar that counts by started_at. Re-activation means new
	// activity, so we move ended_at (not updated_at, which a mere reparse would move without the
	// session having actually changed).
	early := seedSession(t, st, ada, pid, "started-before")
	setSessionShape(t, st, ctx, early, beforeWindow, activeNow, 20, 2)
	insertSignal(t, st, ctx, early, "completed", "A")

	since := now.Add(-7 * 24 * time.Hour)

	// The bar: QualityDistribution counts sessions whose started_at falls in the window.
	dist, err := st.QualityDistribution(ctx, store.AnalyticsFilter{Since: since})
	if err != nil {
		t.Fatalf("quality distribution: %v", err)
	}
	if dist.Sessions != 1 {
		t.Fatalf("windowed bar counted %d sessions, want 1 (only the started-inside session)", dist.Sessions)
	}

	// The drill-down list: ListAllSessions binds Since to s.started_at (unlike ListSessions, which
	// binds it to s.last_active_at), so the same Since value lands on the identical started_at
	// window the bar counted. Its length must equal the bar's count, and it must not include the
	// early-started session.
	rows, _, err := st.ListAllSessions(ctx, store.SessionFilter{Since: since, IncludeEmpty: true})
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(rows) != dist.Sessions {
		t.Fatalf("feed listed %d sessions, want %d (bar and its destination list must agree)", len(rows), dist.Sessions)
	}
	for _, r := range rows {
		if r.ID == early {
			t.Errorf("the early-started session (started_at before the window, active inside it) leaked into the drill-down list")
		}
	}
	if len(rows) != 1 || rows[0].ID != inside {
		t.Errorf("feed = %v, want just the started-inside session %d", rows, inside)
	}

	// A list bound on last-active (Since on last_active_at, the project-page basis via
	// ListSessions) DOES surface the re-activated early session, confirming the two bounds carry
	// distinct semantics and the started_at drill-down deliberately uses started_at, not
	// last-active.
	byActivity, err := st.ListSessions(ctx, store.SessionFilter{Since: since, IncludeEmpty: true})
	if err != nil {
		t.Fatalf("list sessions by activity: %v", err)
	}
	if len(byActivity) != 2 {
		t.Errorf("last-active list showed %d sessions, want 2 (both the inside and the re-activated early session)", len(byActivity))
	}
}

// TestReparseDoesNotFloatLastActive is the regression test for the feed showing days-old
// sessions as "updated" today. last_active_at reads the session's last event time (ended_at),
// not the row's updated_at write time, so a reparse (which restamps updated_at to now but replays
// the same events, leaving ended_at fixed) must not move a session in the last-active list or
// window. This is the exact scenario a bulk epoch reparse produces.
func TestReparseDoesNotFloatLastActive(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	ada := seedUser(t, st, "ada")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour) // last active a month ago

	sid := seedSession(t, st, ada, pid, "old-and-reparsed")
	setSessionShape(t, st, ctx, sid, old, old.Add(10*time.Minute), 20, 2)

	// Simulate a reparse: updated_at jumps to now while the replayed events leave started_at and
	// ended_at (and thus last_active_at) exactly where the session's activity put them.
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET updated_at = now() WHERE id = $1`, sid); err != nil {
		t.Fatalf("bump updated_at to simulate reparse: %v", err)
	}

	// The row reports its month-old activity, not the reparse time.
	rows, err := st.ListSessions(ctx, store.SessionFilter{ProjectID: pid, IncludeEmpty: true})
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("listed %d sessions, want 1", len(rows))
	}
	if rows[0].LastActiveAt == nil {
		t.Fatal("last_active_at is nil, want the session's month-old last-event time")
	}
	if got := *rows[0].LastActiveAt; now.Sub(got) < 7*24*time.Hour {
		t.Errorf("last_active_at = %v (%.0fh ago), want the month-old activity, not the reparse time", got, now.Sub(got).Hours())
	}

	// A 7-day last-active window excludes the reparsed-but-inactive session. Under the old
	// updated_at behavior it would have leaked in, because the reparse restamped updated_at.
	win, err := st.ListSessions(ctx, store.SessionFilter{
		ProjectID: pid, Since: now.Add(-7 * 24 * time.Hour), IncludeEmpty: true,
	})
	if err != nil {
		t.Fatalf("windowed list: %v", err)
	}
	if len(win) != 0 {
		t.Errorf("7-day last-active window listed %d sessions, want 0 (the reparse must not resurface a month-old session)", len(win))
	}
}


// TestInsightsPanelsShareCohort guards the parallel snapshot: Insights runs its panels
// concurrently on separate connections that each import one exported MVCC snapshot, so the
// overlapping denominators (the quality total, the archetype split, and the concurrency
// count) must all describe the identical scoped cohort. If a panel ever read on its own
// snapshot instead of the shared one, these totals could drift apart; this pins them equal
// over a hand-seeded corpus, and exercises the bucket path so the context-plus-trends panel
// and the control-transaction window resolution run too. It then re-reads with the
// quality-band panel set and pins that panel selection changes which groups are computed,
// never the numbers inside the shared core.
func TestInsightsPanelsShareCohort(t *testing.T) {
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

	// Six sessions, all in-window with a span and a fresh signals row, so every panel counts
	// each one: the shared-cohort totals should all read six.
	shape := []struct {
		user  int64
		src   string
		out   string
		grade string
	}{
		{ada, "c1", "completed", "A"},
		{ada, "c2", "completed", "B"},
		{ada, "c3", "errored", "C"},
		{ada, "c4", "abandoned", "D"},
		{grace, "c5", "completed", "A"},
		{grace, "c6", "unknown", ""},
	}
	var first int64
	for i, s := range shape {
		sid := seedSession(t, st, s.user, pid, s.src)
		if first == 0 {
			first = sid
		}
		start := recent.Add(time.Duration(i) * time.Minute)
		setSessionShape(t, st, ctx, sid, start, start.Add(10*time.Minute), 20, 2)
		insertSignal(t, st, ctx, sid, s.out, s.grade)
	}
	// A couple of tool calls on the first session, so the Tools group has data and the
	// band-panel assertions below can tell "computed" from "skipped".
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO tool_calls (session_id, message_ordinal, call_index, tool_name, category, result_status)
		 VALUES ($1, 1, 0, 'Read', 'read', 'ok'), ($1, 1, 1, 'Bash', 'bash', 'error')`, first); err != nil {
		t.Fatalf("seed tool calls: %v", err)
	}

	ins, err := st.Insights(ctx, store.AnalyticsFilter{Since: time.Now().Add(-90 * 24 * time.Hour), Bucket: "day"}, store.AllInsightsPanels)
	if err != nil {
		t.Fatalf("insights: %v", err)
	}

	const want = 6
	if ins.Quality.Sessions != want {
		t.Errorf("Quality.Sessions = %d, want %d", ins.Quality.Sessions, want)
	}
	if ins.Concurrency.Sessions != want {
		t.Errorf("Concurrency.Sessions = %d, want %d (same cohort as the quality total)", ins.Concurrency.Sessions, want)
	}
	archTotal := 0
	for _, a := range ins.Archetypes {
		archTotal += a.Count
	}
	if archTotal != want {
		t.Errorf("archetype total = %d, want %d (every scoped session bands once)", archTotal, want)
	}
	if ins.Trends == nil {
		t.Error("Trends is nil, want a populated grid for the bucketed call")
	}
	if !ins.HasData() {
		t.Error("HasData() = false over a six-session corpus")
	}

	// The quality-band panel set computes the same core (identical quality distribution and
	// signal series: the band's charts must not drift from the fleet page's over one corpus)
	// while leaving the skipped instrument groups zero, so the project page pays for exactly
	// what it mounts.
	band, err := st.Insights(ctx, store.AnalyticsFilter{Since: time.Now().Add(-90 * 24 * time.Hour), Bucket: "day"}, store.QualityBandPanels)
	if err != nil {
		t.Fatalf("insights (band panels): %v", err)
	}
	if band.Quality.Sessions != ins.Quality.Sessions || band.Quality.Graded != ins.Quality.Graded {
		t.Errorf("band quality differs from the full set's: sessions %d/%d graded %d/%d",
			band.Quality.Sessions, ins.Quality.Sessions, band.Quality.Graded, ins.Quality.Graded)
	}
	if band.Concurrency.Sessions != 0 {
		t.Errorf("band panels computed the skipped concurrency group: %+v", band.Concurrency)
	}
	if !band.Tools.HasData() {
		t.Error("band panels skipped Tools, which the band renders")
	}
	if band.Trends == nil {
		t.Fatal("band Trends is nil, want the signal series (the core computes them)")
	}
	if len(band.Trends.Signals.GradeShare) != len(ins.Trends.Signals.GradeShare) {
		t.Errorf("band signal series buckets = %d, want %d (core trends must not depend on the panel set)",
			len(band.Trends.Signals.GradeShare), len(ins.Trends.Signals.GradeShare))
	}
	if len(band.Trends.Gallery.Rows) != 0 || len(band.Trends.Economics.CostCompleted) != 0 {
		t.Errorf("band panels computed skipped trend groups: gallery %d rows, economics %d buckets",
			len(band.Trends.Gallery.Rows), len(band.Trends.Economics.CostCompleted))
	}
}

// TestInsightsBucketedGridUnderConnectionStarvation guards the fully-serial fallback that the
// split trend panels newly stress. When the pool has no connection to spare beyond the one the
// control transaction holds, Insights runs every panel sequentially on that control connection
// rather than block acquiring another (the deadlock-avoidance path panelWorkers gates). The
// trend grid used to be a single panel; it is now nine, so this path piles nine extra panels
// onto the one control connection. Holding the rest of the pool pins every panel there, then
// the assertions confirm the bucketed grid still populates fully and consistently: a starved
// pool must degrade to a slow-but-correct render, never a deadlock or a silently missing chart
// (which is how a trend panel dropped from the fallback dispatch would show up).
func TestInsightsBucketedGridUnderConnectionStarvation(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	ada := seedUser(t, st, "ada")
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	recent := time.Now().Add(-24 * time.Hour)
	for i, sh := range []struct{ out, grade string }{
		{"completed", "A"},
		{"completed", "B"},
		{"abandoned", "D"},
	} {
		sid := seedSession(t, st, ada, pid, fmt.Sprintf("s%d", i))
		start := recent.Add(time.Duration(i) * time.Minute)
		setSessionShape(t, st, ctx, sid, start, start.Add(10*time.Minute), 20, 2)
		insertSignal(t, st, ctx, sid, sh.out, sh.grade)
		// A priced usage event so the money panels (fleet mix, economics) have tokens and cost to
		// read, not just the session-shape panels; $1.50 each, occurring inside the window.
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd, occurred_at, dedup_key)
			 VALUES ($1, 'claude-opus-4', 1000, 500, 0, 0, 1.5, $2, 'u1')`,
			sid, start); err != nil {
			t.Fatalf("seed usage for %d: %v", sid, err)
		}
		deriveUsageRollup(t, st, sid)
	}

	// Hold every connection but one. Insights then acquires the last for its control transaction
	// and AcquireAllIdle finds nothing spare, so panelWorkers routes every panel, distribution
	// and trend alike, sequentially onto that one control connection.
	maxConns := int(st.Pool.Config().MaxConns)
	if maxConns < 2 {
		t.Skipf("pool max conns = %d, need at least 2 to leave a control connection", maxConns)
	}
	var held []*pgxpool.Conn
	for i := 0; i < maxConns-1; i++ {
		c, err := st.Pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("hold pool connection %d: %v", i, err)
		}
		held = append(held, c)
	}
	defer func() {
		for _, c := range held {
			c.Release()
		}
	}()

	ins, err := st.Insights(ctx, store.AnalyticsFilter{Since: time.Now().Add(-90 * 24 * time.Hour), Bucket: "day"}, store.AllInsightsPanels)
	if err != nil {
		t.Fatalf("insights under connection starvation: %v", err)
	}

	// The distributions still read the three-session cohort.
	if ins.Quality.Sessions != 3 {
		t.Errorf("Quality.Sessions = %d, want 3", ins.Quality.Sessions)
	}

	// Every trend panel ran on the shared control connection: each field below is owned by a
	// distinct appended panel, so a zero here would mean that panel was skipped in the fallback.
	tr := ins.Trends
	if tr == nil || !tr.HasData() {
		t.Fatalf("Trends missing or empty under starvation: %+v", tr)
	}
	if !tr.FleetMix.HasData() {
		t.Error("FleetMix empty: the fleet-mix panel did not run on the control connection")
	}
	var outcomeTotal int
	for _, n := range tr.Signals.OutcomeTotal {
		outcomeTotal += n
	}
	if outcomeTotal != 3 {
		t.Errorf("signal outcome total = %d, want 3 (the signal-trend panel ran)", outcomeTotal)
	}
	if got := tr.Economics.TotalSpend; got < 4.49 || got > 4.51 {
		t.Errorf("economics spend = %v, want 4.5 (three sessions at $1.50; the economics panel ran)", got)
	}
	if tr.Gallery.Total != 3 {
		t.Errorf("gallery total = %d, want 3 (the gallery panel ran)", tr.Gallery.Total)
	}
	if len(tr.Velocity.ActiveHours) != len(tr.BucketStarts) {
		t.Errorf("velocity series not grid-shaped: %d active-hour buckets vs %d grid buckets (the velocity panel ran)", len(tr.Velocity.ActiveHours), len(tr.BucketStarts))
	}
	// Rhythm and Subagents are appended last, so a fallback loop that stopped early would drop
	// them. Neither has seeded input here (no messages, no subagents), so assert the always-
	// allocated shape their builders return rather than volume: the 7-day rhythm grid and the
	// grid-length delegation series both prove their panel reached the end of the dispatch.
	if len(tr.Rhythm.Cells) != 7 {
		t.Errorf("rhythm grid has %d day rows, want 7 (the rhythm panel ran)", len(tr.Rhythm.Cells))
	}
	if len(tr.Subagents.DelegateShare) != len(tr.BucketStarts) {
		t.Errorf("subagent series not grid-shaped: %d vs %d buckets (the subagents panel, appended last, ran)", len(tr.Subagents.DelegateShare), len(tr.BucketStarts))
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
	mk("quick", 1, 4, 1)           // quick
	mk("standard-msgs", 2, 20, 2)  // standard by message count
	mk("deep-msgs", 2, 80, 3)      // deep by message count
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

// TestVelocityStats pins the cadence figures against a hand-computed timeline: two clean
// turns and one with a long idle gap give known prompt-to-reply latencies, a capped active
// span, and message and tool counts, so the percentiles, the first-response figure, and
// the per-active-minute rates are all exact. It also confirms the window and per-user
// scoping narrow the same way the rest of the Insights page does.
func TestVelocityStats(t *testing.T) {
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

	// Session one (Ada): two turns. Turn one replies 10s after the prompt (also the
	// opening reply); turn two replies 30s after its prompt. Gaps 10, 10, 40, 30 are all
	// under the active cap, so the active span is 90s; five messages, two tool calls.
	b1 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	s1 := seedSession(t, st, ada, pid, "v1")
	rebuildWith(t, st, s1, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "go", Timestamp: b1},
			{Ordinal: 1, Role: "assistant", Content: "on it", HasToolUse: true, Timestamp: b1.Add(10 * time.Second)},
			{Ordinal: 2, Role: "assistant", Content: "more", Timestamp: b1.Add(20 * time.Second)},
			{Ordinal: 3, Role: "user", Content: "next", Timestamp: b1.Add(60 * time.Second)},
			{Ordinal: 4, Role: "assistant", Content: "done", Timestamp: b1.Add(90 * time.Second)},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Read", Category: "read", CallUID: "a"},
			{MessageOrdinal: 1, CallIndex: 1, ToolName: "Edit", Category: "edit", FilePath: "x.go", CallUID: "b"},
		},
	})
	setSessionShape(t, st, ctx, s1, recent, recent.Add(2*time.Minute), 5, 2)

	// Session two (Ada): turn one replies 20s after the prompt (the opening reply); then a
	// one-hour idle gap before turn two, whose reply lands 50s after its prompt. The idle
	// gap is over the cap and drops out, so the active span is 20+50 = 70s; four messages,
	// one tool call.
	b2 := time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)
	s2 := seedSession(t, st, ada, pid, "v2")
	rebuildWith(t, st, s2, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "start", Timestamp: b2},
			{Ordinal: 1, Role: "assistant", Content: "reply", HasToolUse: true, Timestamp: b2.Add(20 * time.Second)},
			{Ordinal: 2, Role: "user", Content: "back", Timestamp: b2.Add(3600 * time.Second)},
			{Ordinal: 3, Role: "assistant", Content: "ok", Timestamp: b2.Add(3650 * time.Second)},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Bash", Category: "bash", CallUID: "c"},
		},
	})
	setSessionShape(t, st, ctx, s2, recent, recent.Add(70*time.Minute), 4, 2)

	// Session three (Grace): a single 100s turn, started long ago so a trailing window
	// drops it. It is the only non-Ada session, so per-user scoping drops it too.
	b3 := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)
	s3 := seedSession(t, st, grace, pid, "v3")
	rebuildWith(t, st, s3, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "hello", Timestamp: b3},
			{Ordinal: 1, Role: "assistant", Content: "hi", Timestamp: b3.Add(100 * time.Second)},
		},
	})
	setSessionShape(t, st, ctx, s3, old, old.Add(2*time.Minute), 2, 1)

	// Ada only: latencies are [10, 30, 20, 50]. percentile_cont gives p50 = 25s, p90 = 44s;
	// the opening replies are [10, 20] so their median is 15s. Active span 90+70 = 160s =
	// 2.6667 min over 9 messages and 3 tool calls.
	ada1 := store.AnalyticsFilter{Username: "ada"}
	v, err := st.VelocityStats(ctx, ada1)
	if err != nil {
		t.Fatalf("velocity (ada): %v", err)
	}
	if v.Turns != 4 || v.Sessions != 2 {
		t.Errorf("ada turns/sessions = %d/%d, want 4/2", v.Turns, v.Sessions)
	}
	if v.ResponseP50 != 25*time.Second {
		t.Errorf("ResponseP50 = %s, want 25s", v.ResponseP50)
	}
	if v.ResponseP90 != 44*time.Second {
		t.Errorf("ResponseP90 = %s, want 44s", v.ResponseP90)
	}
	if v.FirstResponseP50 != 15*time.Second {
		t.Errorf("FirstResponseP50 = %s, want 15s", v.FirstResponseP50)
	}
	if math.Abs(v.ActiveSeconds-160) > 0.001 {
		t.Errorf("ActiveSeconds = %.3f, want 160", v.ActiveSeconds)
	}
	if wantMsgs := 9.0 / (160.0 / 60.0); math.Abs(v.MsgsPerActiveMin-wantMsgs) > 0.001 {
		t.Errorf("MsgsPerActiveMin = %.4f, want %.4f", v.MsgsPerActiveMin, wantMsgs)
	}
	if wantTools := 3.0 / (160.0 / 60.0); math.Abs(v.ToolsPerActiveMin-wantTools) > 0.001 {
		t.Errorf("ToolsPerActiveMin = %.4f, want %.4f", v.ToolsPerActiveMin, wantTools)
	}

	// Unscoped over all time: Grace's session joins, so five turns across three sessions.
	all, err := st.VelocityStats(ctx, store.AnalyticsFilter{})
	if err != nil {
		t.Fatalf("velocity (all): %v", err)
	}
	if all.Turns != 5 || all.Sessions != 3 {
		t.Errorf("all turns/sessions = %d/%d, want 5/3", all.Turns, all.Sessions)
	}

	// A trailing window keyed on started_at drops Grace's old session, leaving Ada's two.
	windowed, err := st.VelocityStats(ctx, store.AnalyticsFilter{Since: time.Now().Add(-90 * 24 * time.Hour)})
	if err != nil {
		t.Fatalf("velocity (windowed): %v", err)
	}
	if windowed.Turns != 4 || windowed.Sessions != 2 {
		t.Errorf("windowed turns/sessions = %d/%d, want 4/2", windowed.Turns, windowed.Sessions)
	}
}

// TestVelocityStatsEdges pins the turn model at the cases the SQL is most likely to get
// wrong: consecutive prompts before a reply, a reply whose clock drifted before its
// prompt, an undated prompt that must not lend its reply to the previous turn, and a
// session that opens with assistant preamble. Each case is its own user so a per-user
// scope isolates it.
func TestVelocityStatsEdges(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	at := func(sec int) time.Time { return base.Add(time.Duration(sec) * time.Second) }

	apply := func(user, src string, msgs []store.MessageDelta) store.AnalyticsFilter {
		uid := seedUser(t, st, user)
		sid := seedSession(t, st, uid, pid, src)
		rebuildWith(t, st, sid, store.ProjectionDelta{Messages: msgs})
		return store.AnalyticsFilter{Username: user}
	}

	// Consecutive prompts: turn one (the first prompt) draws no reply, so it is not
	// measured; turn two replies 5s later. The opening-reply figure stays empty because the
	// session's first turn never got a reply.
	consec := apply("consec", "e-consec", []store.MessageDelta{
		{Ordinal: 0, Role: "user", Content: "first", Timestamp: at(0)},
		{Ordinal: 1, Role: "user", Content: "second", Timestamp: at(10)},
		{Ordinal: 2, Role: "assistant", Content: "reply", Timestamp: at(15)},
	})
	if v, err := st.VelocityStats(ctx, consec); err != nil {
		t.Fatalf("consec velocity: %v", err)
	} else if v.Turns != 1 || v.ResponseP50 != 5*time.Second || v.FirstResponseP50 != 0 {
		t.Errorf("consec = {turns %d, p50 %s, opening %s}, want {1, 5s, 0}", v.Turns, v.ResponseP50, v.FirstResponseP50)
	}

	// Clock skew: the reply timestamp precedes the prompt, so the turn is rejected rather
	// than counted as a negative latency.
	skew := apply("skew", "e-skew", []store.MessageDelta{
		{Ordinal: 0, Role: "user", Content: "go", Timestamp: at(20)},
		{Ordinal: 1, Role: "assistant", Content: "early", Timestamp: at(10)},
	})
	if v, err := st.VelocityStats(ctx, skew); err != nil {
		t.Fatalf("skew velocity: %v", err)
	} else if v.Turns != 0 {
		t.Errorf("skew turns = %d, want 0 (a reply before its prompt is not a latency)", v.Turns)
	}

	// Undated prompt: the first prompt got no reply, and a second, undated prompt drew the
	// only reply. The reply must attach to the undated turn (which has no measurable
	// latency), NOT to the first prompt as a false 50s latency. So nothing is measured, but
	// the two timestamped messages still register active time.
	nullreply := apply("nullreply", "e-nullreply", []store.MessageDelta{
		{Ordinal: 0, Role: "user", Content: "first", Timestamp: at(0)},
		{Ordinal: 1, Role: "user", Content: "undated"},
		{Ordinal: 2, Role: "assistant", Content: "reply", Timestamp: at(50)},
	})
	if v, err := st.VelocityStats(ctx, nullreply); err != nil {
		t.Fatalf("nullreply velocity: %v", err)
	} else if v.Turns != 0 || v.ActiveSeconds != 50 {
		t.Errorf("nullreply = {turns %d, active %.0f}, want {0, 50} (no false latency, gap still active)", v.Turns, v.ActiveSeconds)
	}

	// Assistant preamble: the session opens with an assistant message (turn zero, before
	// any prompt), which is skipped; the first human turn replies 7s later and is both the
	// only turn and the opening reply.
	asstFirst := apply("asstfirst", "e-asstfirst", []store.MessageDelta{
		{Ordinal: 0, Role: "assistant", Content: "preamble", Timestamp: at(0)},
		{Ordinal: 1, Role: "user", Content: "go", Timestamp: at(5)},
		{Ordinal: 2, Role: "assistant", Content: "done", Timestamp: at(12)},
	})
	if v, err := st.VelocityStats(ctx, asstFirst); err != nil {
		t.Fatalf("asstfirst velocity: %v", err)
	} else if v.Turns != 1 || v.ResponseP50 != 7*time.Second || v.FirstResponseP50 != 7*time.Second {
		t.Errorf("asstfirst = {turns %d, p50 %s, opening %s}, want {1, 7s, 7s}", v.Turns, v.ResponseP50, v.FirstResponseP50)
	}

	// Reply by ordinal, not by clock: one turn with two assistant messages, the first at 10s
	// and a second whose clock drifted back to 4s. The latency is the FIRST reply by ordinal
	// (10s), not the earliest timestamp (4s), so a drifted later row cannot understate the
	// wait. This pins the asst_turns DISTINCT ON ... ORDER BY ordinal that replaced the old
	// array_agg[1]: both pick the first reply by position, never the minimum instant.
	drift := apply("drift", "e-drift", []store.MessageDelta{
		{Ordinal: 0, Role: "user", Content: "go", Timestamp: at(0)},
		{Ordinal: 1, Role: "assistant", Content: "first reply", Timestamp: at(10)},
		{Ordinal: 2, Role: "assistant", Content: "second reply, clock drifted back", Timestamp: at(4)},
	})
	if v, err := st.VelocityStats(ctx, drift); err != nil {
		t.Fatalf("drift velocity: %v", err)
	} else if v.Turns != 1 || v.ResponseP50 != 10*time.Second || v.FirstResponseP50 != 10*time.Second {
		t.Errorf("drift = {turns %d, p50 %s, opening %s}, want {1, 10s, 10s} (first reply by ordinal, not earliest clock)",
			v.Turns, v.ResponseP50, v.FirstResponseP50)
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
