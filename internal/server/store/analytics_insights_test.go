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
	markSignalsFresh(t, st, ctx, sid)
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
	mk(ua, "a4", recent, quality.Version, "unknown", "")             // explicitly unscored
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

// TestUserQuality confirms the per-user leaderboard the Insights People panel reads: the
// per-author session counts, the outcome partition (Unknown as the residue), the graded
// coverage, the average score over scored rows only, and the busiest-first ordering. It goes
// through the Insights snapshot (userQualityFrom is unexported and threaded only there), which
// also exercises that the panel shares the quality total's cohort. It confirms the same run's
// QualityDistribution.Graded, the coverage figure the Grades panel notes.
func TestUserQuality(t *testing.T) {
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

	// Score comes from insertSignal's fixed 80 for any graded row, so the average over a user's
	// graded sessions is 80 when they have any and nil when they have none. mk stamps a shape so
	// the session is windowed, then a signals row (empty grade = the unscored/unknown bucket).
	mk := func(user int64, src, outcome, grade string) int64 {
		sid := seedSession(t, st, user, pid, src)
		setSessionShape(t, st, ctx, sid, recent, recent.Add(10*time.Minute), 20, 2)
		insertSignal(t, st, ctx, sid, quality.Version, outcome, grade)
		return sid
	}
	// Ada: three graded (two completed A/B, one errored C) plus one ungraded/unknown session,
	// so four sessions, three graded, and an outcome mix of 2 completed / 1 errored / 1 unknown.
	mk(ada, "a1", "completed", "A")
	mk(ada, "a2", "completed", "B")
	mk(ada, "a3", "errored", "C")
	// An ungraded session (no signals row at all): still counted under Ada, unknown outcome.
	noneID := seedSession(t, st, ada, pid, "a4none")
	setSessionShape(t, st, ctx, noneID, recent, recent.Add(10*time.Minute), 20, 2)
	// Grace: one abandoned, graded D.
	mk(grace, "g1", "abandoned", "D")

	since := time.Now().Add(-90 * 24 * time.Hour)
	ins, err := st.Insights(ctx, store.AnalyticsFilter{Since: since})
	if err != nil {
		t.Fatalf("insights: %v", err)
	}

	// Graded coverage on the distribution: 4 of 5 sessions carry a grade (Ada's a4none is the
	// one unscored), so Graded is 4 against Sessions 5.
	if ins.Quality.Sessions != 5 || ins.Quality.Graded != 4 {
		t.Errorf("quality sessions/graded = %d/%d, want 5/4", ins.Quality.Sessions, ins.Quality.Graded)
	}

	users := ins.Users.Users
	if len(users) != 2 {
		t.Fatalf("users = %d, want 2", len(users))
	}
	// Busiest first: Ada (4) before Grace (1).
	if users[0].Username != "ada" || users[1].Username != "grace" {
		t.Fatalf("order = %s, %s, want ada, grace", users[0].Username, users[1].Username)
	}
	ada0 := users[0]
	if ada0.Sessions != 4 || ada0.Graded != 3 {
		t.Errorf("ada sessions/graded = %d/%d, want 4/3", ada0.Sessions, ada0.Graded)
	}
	if ada0.Completed != 2 || ada0.Errored != 1 || ada0.Abandoned != 0 || ada0.Unknown != 1 {
		t.Errorf("ada outcome mix = c%d a%d e%d u%d, want c2 a0 e1 u1",
			ada0.Completed, ada0.Abandoned, ada0.Errored, ada0.Unknown)
	}
	if ada0.Completed+ada0.Abandoned+ada0.Errored+ada0.Unknown != ada0.Sessions {
		t.Errorf("ada outcome counts do not partition sessions: %+v", ada0)
	}
	if ada0.AvgScore == nil || *ada0.AvgScore != 80.0 {
		t.Errorf("ada avg score = %v, want 80.0", ada0.AvgScore)
	}
	grace0 := users[1]
	if grace0.Sessions != 1 || grace0.Graded != 1 || grace0.Abandoned != 1 {
		t.Errorf("grace = {sessions %d, graded %d, abandoned %d}, want {1, 1, 1}", grace0.Sessions, grace0.Graded, grace0.Abandoned)
	}
}

// TestUserQualityAvgScoreNilWhenUnscored confirms an author with sessions but no scored
// session in scope reports a nil AvgScore (the "unmeasured" default the panel dashes), not a
// zero that would read as a real failing average.
func TestUserQualityAvgScoreNilWhenUnscored(t *testing.T) {
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

	// Ada: two ungraded/unknown sessions (explicit unscored rows). Grace: one graded so the
	// panel has two authors and renders.
	for _, src := range []string{"a1", "a2"} {
		sid := seedSession(t, st, ada, pid, src)
		setSessionShape(t, st, ctx, sid, recent, recent.Add(10*time.Minute), 20, 2)
		insertSignal(t, st, ctx, sid, quality.Version, "unknown", "")
	}
	g := seedSession(t, st, grace, pid, "g1")
	setSessionShape(t, st, ctx, g, recent, recent.Add(10*time.Minute), 20, 2)
	insertSignal(t, st, ctx, g, quality.Version, "completed", "A")

	ins, err := st.Insights(ctx, store.AnalyticsFilter{Since: time.Now().Add(-90 * 24 * time.Hour)})
	if err != nil {
		t.Fatalf("insights: %v", err)
	}
	var ada0 store.UserQuality
	for _, u := range ins.Users.Users {
		if u.Username == "ada" {
			ada0 = u
		}
	}
	if ada0.Username != "ada" {
		t.Fatalf("ada not in users: %+v", ins.Users.Users)
	}
	if ada0.AvgScore != nil {
		t.Errorf("ada avg score = %v, want nil (no scored session)", ada0.AvgScore)
	}
	if ada0.Sessions != 2 || ada0.Graded != 0 || ada0.Unknown != 2 {
		t.Errorf("ada = {sessions %d, graded %d, unknown %d}, want {2, 0, 2}", ada0.Sessions, ada0.Graded, ada0.Unknown)
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
	if err := st.ApplyProjectionDelta(ctx, s1, store.ProjectionDelta{
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
	}); err != nil {
		t.Fatalf("apply s1: %v", err)
	}
	setSessionShape(t, st, ctx, s1, recent, recent.Add(2*time.Minute), 5, 2)

	// Session two (Ada): turn one replies 20s after the prompt (the opening reply); then a
	// one-hour idle gap before turn two, whose reply lands 50s after its prompt. The idle
	// gap is over the cap and drops out, so the active span is 20+50 = 70s; four messages,
	// one tool call.
	b2 := time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)
	s2 := seedSession(t, st, ada, pid, "v2")
	if err := st.ApplyProjectionDelta(ctx, s2, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "start", Timestamp: b2},
			{Ordinal: 1, Role: "assistant", Content: "reply", HasToolUse: true, Timestamp: b2.Add(20 * time.Second)},
			{Ordinal: 2, Role: "user", Content: "back", Timestamp: b2.Add(3600 * time.Second)},
			{Ordinal: 3, Role: "assistant", Content: "ok", Timestamp: b2.Add(3650 * time.Second)},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Bash", Category: "bash", CallUID: "c"},
		},
	}); err != nil {
		t.Fatalf("apply s2: %v", err)
	}
	setSessionShape(t, st, ctx, s2, recent, recent.Add(70*time.Minute), 4, 2)

	// Session three (Grace): a single 100s turn, started long ago so a trailing window
	// drops it. It is the only non-Ada session, so per-user scoping drops it too.
	b3 := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)
	s3 := seedSession(t, st, grace, pid, "v3")
	if err := st.ApplyProjectionDelta(ctx, s3, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "hello", Timestamp: b3},
			{Ordinal: 1, Role: "assistant", Content: "hi", Timestamp: b3.Add(100 * time.Second)},
		},
	}); err != nil {
		t.Fatalf("apply s3: %v", err)
	}
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
		if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{Messages: msgs}); err != nil {
			t.Fatalf("apply %s: %v", src, err)
		}
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
