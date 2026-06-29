package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// seedUsage inserts a session and a usage event directly, bypassing the ingest
// pipeline, so the analytics rollups can be asserted against known inputs.
func seedSessionWithStats(t *testing.T, st *store.Store, userID, projectID int64, agent, src string, cost float64, in, out int64) int64 {
	t.Helper()
	var id int64
	err := st.Pool.QueryRow(context.Background(),
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine,
		        total_cost_usd, total_input_tokens, total_output_tokens)
		 VALUES ($1,$2,$3,$4,'box',$5,$6,$7) RETURNING id`,
		userID, projectID, agent, src, cost, in, out).Scan(&id)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return id
}

func seedUsage(t *testing.T, st *store.Store, sessionID int64, model string, cost float64, in, out int64, daysAgo int, dedup string) {
	t.Helper()
	_, err := st.Pool.Exec(context.Background(),
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1,$2,$3,$4,$5, now() - make_interval(days => $6), $7)`,
		sessionID, model, in, out, cost, daysAgo, dedup)
	if err != nil {
		t.Fatalf("seed usage: %v", err)
	}
}

// seedUsageCache is seedUsage with the two cache-token classes set, so the cache
// totals the overview's Tokens tooltip surfaces can be asserted against known
// inputs.
func seedUsageCache(t *testing.T, st *store.Store, sessionID int64, model string, cost float64, in, out, cacheRead, cacheWrite int64, daysAgo int, dedup string) {
	t.Helper()
	_, err := st.Pool.Exec(context.Background(),
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1,$2,$3,$4,$5,$6,$7, now() - make_interval(days => $8), $9)`,
		sessionID, model, in, out, cacheRead, cacheWrite, cost, daysAgo, dedup)
	if err != nil {
		t.Fatalf("seed usage cache: %v", err)
	}
}

// seedUsageAt inserts a usage event at an explicit occurred_at, so the window's
// inclusive lower bound (`occurred_at >= since`) can be pinned to the exact
// instant rather than a clearly-inside or clearly-outside day.
func seedUsageAt(t *testing.T, st *store.Store, sessionID int64, model string, cost float64, in, out int64, at time.Time, dedup string) {
	t.Helper()
	_, err := st.Pool.Exec(context.Background(),
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
		sessionID, model, in, out, cost, at, dedup)
	if err != nil {
		t.Fatalf("seed usage at: %v", err)
	}
}

// TotalTokens sums the four token classes; it is the figure the overview's Tokens
// readout shows. Pure, so it runs without a database.
func TestAnalyticsTotalTokens(t *testing.T) {
	t.Parallel()
	a := store.Analytics{TotalIn: 100, TotalOut: 50, TotalCacheRead: 30, TotalCacheWrite: 7}
	if got := a.TotalTokens(); got != 187 {
		t.Errorf("TotalTokens = %d, want 187 (100+50+30+7)", got)
	}
	if got := (store.Analytics{}).TotalTokens(); got != 0 {
		t.Errorf("empty TotalTokens = %d, want 0", got)
	}
}

func TestAnalyticsRollups(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	s1 := seedSessionWithStats(t, st, admin.ID, proj, "claude", "s1", 3.0, 1000, 200)
	s2 := seedSessionWithStats(t, st, admin.ID, proj, "codex", "s2", 1.0, 400, 80)

	// Two models, three distinct days.
	seedUsage(t, st, s1, "claude-opus-4-8", 1.5, 500, 100, 0, "u1")
	seedUsage(t, st, s1, "claude-opus-4-8", 1.5, 500, 100, 1, "u2")
	seedUsage(t, st, s2, "gpt-5.5", 1.0, 400, 80, 2, "u3")

	a, err := st.Analytics(ctx, proj, time.Time{}, nil)
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}
	if len(a.Series) != 3 {
		t.Errorf("want 3 daily points, got %d", len(a.Series))
	}
	var seriesCost float64
	for _, p := range a.Series {
		seriesCost += p.CostUSD
	}
	if seriesCost < 3.99 || seriesCost > 4.01 {
		t.Errorf("series cost should sum the usage events (~4.0), got %.2f", seriesCost)
	}
	// Totals come from the session rollups: 3.0 + 1.0.
	if a.TotalCost < 3.99 || a.TotalCost > 4.01 {
		t.Errorf("total cost from session rollups should be ~4.0, got %.2f", a.TotalCost)
	}
	if a.Sessions != 2 {
		t.Errorf("want 2 sessions, got %d", a.Sessions)
	}
	if len(a.Models) != 2 || a.Models[0].Label != "claude-opus-4-8" {
		t.Errorf("models breakdown should be sorted by cost desc: %+v", a.Models)
	}
	if len(a.Agents) != 2 || a.Agents[0].Label != "claude" {
		t.Errorf("agents breakdown should lead with claude (higher cost): %+v", a.Agents)
	}
	if !a.HasData() {
		t.Error("HasData should be true with sessions present")
	}

	// Global scope (projectID 0) sees the same single project.
	g, err := st.Analytics(ctx, 0, time.Time{}, nil)
	if err != nil {
		t.Fatalf("global analytics: %v", err)
	}
	if g.Sessions != 2 || len(g.Series) != 3 {
		t.Errorf("global rollup mismatch: %+v", g)
	}
}

// A non-empty userIDs scopes every rollup to the named users' sessions, leaving
// other users' usage out of the series, the breakdowns, and the totals. It
// exercises both the unbounded by-agent path (reads the session rollups) and the
// windowed path (slices usage_events), and confirms an empty selection is the
// unscoped "all users" view.
func TestAnalyticsUserFilter(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	// Two accounts, each with a session and recent usage on the same project.
	graceID := seedUser(t, st, "grace")
	adaID := seedUser(t, st, "ada")
	proj, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	sg := seedSessionWithStats(t, st, graceID, proj, "claude", "sg", 3.0, 900, 180)
	sa := seedSessionWithStats(t, st, adaID, proj, "codex", "sa", 1.0, 300, 60)
	seedUsage(t, st, sg, "claude-opus-4-8", 3.0, 900, 180, 1, "g1")
	seedUsage(t, st, sa, "gpt-5.5", 1.0, 300, 60, 1, "a1")

	// Scoped to grace, all-time: only her session, spend, and agent survive.
	g, err := st.Analytics(ctx, 0, time.Time{}, []int64{graceID})
	if err != nil {
		t.Fatalf("grace all-time analytics: %v", err)
	}
	if g.Sessions != 1 {
		t.Errorf("grace scope should see only her session, got %d", g.Sessions)
	}
	if g.TotalCost < 2.99 || g.TotalCost > 3.01 {
		t.Errorf("grace scope cost should be ~3.0 from session rollups, got %.2f", g.TotalCost)
	}
	if len(g.Agents) != 1 || g.Agents[0].Label != "claude" {
		t.Errorf("grace scope agents should hold only claude: %+v", g.Agents)
	}
	if len(g.Models) != 1 || g.Models[0].Label != "claude-opus-4-8" {
		t.Errorf("grace scope models should hold only her model: %+v", g.Models)
	}

	// Scoped to grace, windowed: the usage_events path agrees with the rollup path.
	since := time.Now().AddDate(0, 0, -7)
	gw, err := st.Analytics(ctx, 0, since, []int64{graceID})
	if err != nil {
		t.Fatalf("grace windowed analytics: %v", err)
	}
	if gw.Sessions != 1 || gw.TotalIn != 900 || gw.TotalOut != 180 {
		t.Errorf("grace windowed scope wrong: sessions=%d in=%d out=%d", gw.Sessions, gw.TotalIn, gw.TotalOut)
	}

	// Both users selected matches the unscoped view: two sessions, full spend.
	both, err := st.Analytics(ctx, 0, time.Time{}, []int64{graceID, adaID})
	if err != nil {
		t.Fatalf("both-user analytics: %v", err)
	}
	all, err := st.Analytics(ctx, 0, time.Time{}, nil)
	if err != nil {
		t.Fatalf("unscoped analytics: %v", err)
	}
	if both.Sessions != all.Sessions || both.Sessions != 2 {
		t.Errorf("selecting every user should match the unscoped view (2 sessions): both=%d all=%d", both.Sessions, all.Sessions)
	}
	if both.TotalCost < 3.99 || both.TotalCost > 4.01 {
		t.Errorf("both-user cost should sum both sessions (~4.0), got %.2f", both.TotalCost)
	}
}

// A project scope and a user scope apply together: the placeholders are numbered
// in order ($1 project, $2 users, then $3 since on the windowed path), so the
// analytics isolate one user's sessions within one project and exclude both that
// user's other projects and other users in the same project. This pins the
// combined WHERE construction the single-axis tests leave unexercised, on both the
// windowed usage_events path and the unbounded session-rollup path.
func TestAnalyticsProjectAndUserScope(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	graceID := seedUser(t, st, "grace")
	adaID := seedUser(t, st, "ada")
	projA, err := st.UpsertProject(ctx, "github.com/ada/a", "github.com", "ada", "a", "a", "remote")
	if err != nil {
		t.Fatalf("project a: %v", err)
	}
	projB, err := st.UpsertProject(ctx, "github.com/ada/b", "github.com", "ada", "b", "b", "remote")
	if err != nil {
		t.Fatalf("project b: %v", err)
	}

	// grace works in both projects; ada works in project A. Only grace's project A
	// session should survive the combined scope.
	gA := seedSessionWithStats(t, st, graceID, projA, "claude", "gA", 2.0, 200, 40)
	gB := seedSessionWithStats(t, st, graceID, projB, "pi", "gB", 5.0, 500, 100)
	aA := seedSessionWithStats(t, st, adaID, projA, "codex", "aA", 9.0, 900, 180)
	seedUsage(t, st, gA, "claude-opus-4-8", 2.0, 200, 40, 1, "gA1")
	seedUsage(t, st, gB, "pi-1", 5.0, 500, 100, 1, "gB1")
	seedUsage(t, st, aA, "gpt-5.5", 9.0, 900, 180, 1, "aA1")

	assertGraceProjA := func(label string, a store.Analytics) {
		if a.Sessions != 1 {
			t.Errorf("%s: combined scope should see only grace's project A session, got %d", label, a.Sessions)
		}
		if a.TotalCost < 1.99 || a.TotalCost > 2.01 {
			t.Errorf("%s: combined scope cost should be ~2.0 (gA only), got %.2f", label, a.TotalCost)
		}
		if len(a.Agents) != 1 || a.Agents[0].Label != "claude" {
			t.Errorf("%s: combined scope agents should hold only claude (not ada's codex, not grace's pi): %+v", label, a.Agents)
		}
	}

	// Windowed path (usage_events): project A AND grace.
	since := time.Now().AddDate(0, 0, -7)
	w, err := st.Analytics(ctx, projA, since, []int64{graceID})
	if err != nil {
		t.Fatalf("windowed combined analytics: %v", err)
	}
	assertGraceProjA("windowed", w)
	if w.TotalIn != 200 || w.TotalOut != 40 {
		t.Errorf("windowed combined token totals wrong: in=%d out=%d, want 200/40", w.TotalIn, w.TotalOut)
	}

	// Unbounded path (session rollups): project A AND grace.
	all, err := st.Analytics(ctx, projA, time.Time{}, []int64{graceID})
	if err != nil {
		t.Fatalf("all-time combined analytics: %v", err)
	}
	assertGraceProjA("all-time", all)
}

// ListUsers returns every account ordered by username, carrying only the identity
// (id and username) and never the credential.
func TestListUsers(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	// Inserted out of alphabetical order to prove the query, not the insert order,
	// sets the result order.
	seedUser(t, st, "grace")
	seedUser(t, st, "ada")
	seedUser(t, st, "katherine")

	users, err := st.ListUsers(ctx)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	var names []string
	for _, u := range users {
		names = append(names, u.Username)
		if u.ID == 0 {
			t.Errorf("user %q has zero id", u.Username)
		}
		if u.PasswordHash != "" {
			t.Errorf("ListUsers should not carry the password hash, got %q for %q", u.PasswordHash, u.Username)
		}
	}
	if got := strings.Join(names, ","); got != "ada,grace,katherine" {
		t.Errorf("ListUsers order = %q, want ada,grace,katherine", got)
	}
}

// seedUser inserts an account directly and returns its id, so a test can own
// sessions by distinct users without driving the invite-gated registration flow.
func seedUser(t *testing.T, st *store.Store, username string) int64 {
	t.Helper()
	var id int64
	if err := st.Pool.QueryRow(context.Background(),
		`INSERT INTO users (username, password_hash, is_admin) VALUES ($1, 'x', FALSE) RETURNING id`,
		username).Scan(&id); err != nil {
		t.Fatalf("seed user %q: %v", username, err)
	}
	return id
}

// A non-zero `since` bounds every rollup to the trailing window, slicing usage by
// event time. Only events at or after the bound count toward the series, the
// breakdowns, the totals, and the distinct-session count.
func TestAnalyticsTimeWindow(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	// s1 is active inside the window; s2 only has activity well before it.
	s1 := seedSessionWithStats(t, st, admin.ID, proj, "claude", "s1", 2.0, 600, 120)
	s2 := seedSessionWithStats(t, st, admin.ID, proj, "codex", "s2", 9.0, 400, 80)
	seedUsage(t, st, s1, "claude-opus-4-8", 1.0, 300, 60, 0, "in1")
	seedUsage(t, st, s1, "claude-opus-4-8", 1.0, 300, 60, 3, "in2")
	seedUsage(t, st, s2, "gpt-5.5", 9.0, 400, 80, 40, "old")

	// A 7-day window keeps only s1's two recent events.
	since := time.Now().AddDate(0, 0, -7)
	a, err := st.Analytics(ctx, 0, since, nil)
	if err != nil {
		t.Fatalf("windowed analytics: %v", err)
	}
	if len(a.Series) != 2 {
		t.Errorf("want 2 in-window daily points, got %d", len(a.Series))
	}
	if a.TotalCost < 1.99 || a.TotalCost > 2.01 {
		t.Errorf("windowed cost should sum only in-window events (~2.0), got %.2f", a.TotalCost)
	}
	if a.Sessions != 1 {
		t.Errorf("only s1 is active in-window, want 1 session, got %d", a.Sessions)
	}
	if a.TotalIn != 600 || a.TotalOut != 120 {
		t.Errorf("windowed token totals wrong: in=%d out=%d", a.TotalIn, a.TotalOut)
	}
	if len(a.Models) != 1 || a.Models[0].Label != "claude-opus-4-8" {
		t.Errorf("windowed models should hold only the in-window model: %+v", a.Models)
	}
	if len(a.Agents) != 1 || a.Agents[0].Label != "claude" {
		t.Errorf("windowed agents should hold only the in-window agent: %+v", a.Agents)
	}

	// The unbounded view still sees both sessions and the older spend.
	full, err := st.Analytics(ctx, 0, time.Time{}, nil)
	if err != nil {
		t.Fatalf("full analytics: %v", err)
	}
	if full.Sessions != 2 {
		t.Errorf("unbounded view should see both sessions, got %d", full.Sessions)
	}
	if full.TotalCost < 10.99 || full.TotalCost > 11.01 {
		t.Errorf("unbounded cost from session rollups should be ~11.0, got %.2f", full.TotalCost)
	}
}

// A project scope and a time bound apply together: the placeholders are numbered
// in order ($1 project, $2 since), so the analytics isolate one project's
// in-window usage and exclude both another project and out-of-window events. This
// also exercises the cache-token totals the unscoped window test leaves at zero.
func TestAnalyticsScopedWindowWithCacheTotals(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projA, err := st.UpsertProject(ctx, "github.com/ada/a", "github.com", "ada", "a", "a", "remote")
	if err != nil {
		t.Fatalf("project a: %v", err)
	}
	projB, err := st.UpsertProject(ctx, "github.com/ada/b", "github.com", "ada", "b", "b", "remote")
	if err != nil {
		t.Fatalf("project b: %v", err)
	}

	sA := seedSessionWithStats(t, st, admin.ID, projA, "claude", "sa", 1.0, 100, 50)
	sB := seedSessionWithStats(t, st, admin.ID, projB, "codex", "sb", 9.0, 999, 999)

	// Project A, in window: the only events that should count.
	seedUsageCache(t, st, sA, "claude-opus-4-8", 1.0, 100, 50, 30, 7, 1, "a-recent")
	// Project A, out of a 7-day window: excluded by the time bound.
	seedUsageCache(t, st, sA, "claude-opus-4-8", 4.0, 400, 80, 200, 20, 40, "a-old")
	// Project B, in window: excluded by the project scope.
	seedUsageCache(t, st, sB, "gpt-5.5", 9.0, 999, 999, 999, 999, 1, "b-recent")

	since := time.Now().AddDate(0, 0, -7)
	a, err := st.Analytics(ctx, projA, since, nil)
	if err != nil {
		t.Fatalf("scoped windowed analytics: %v", err)
	}
	if a.TotalIn != 100 || a.TotalOut != 50 {
		t.Errorf("scoped window in/out wrong: in=%d out=%d, want 100/50", a.TotalIn, a.TotalOut)
	}
	if a.TotalCacheRead != 30 || a.TotalCacheWrite != 7 {
		t.Errorf("scoped window cache totals wrong: read=%d write=%d, want 30/7", a.TotalCacheRead, a.TotalCacheWrite)
	}
	if got := a.TotalTokens(); got != 187 {
		t.Errorf("scoped window combined tokens = %d, want 187 (100+50+30+7)", got)
	}
	if a.TotalCost < 0.99 || a.TotalCost > 1.01 {
		t.Errorf("scoped window cost should be the one in-window event (~1.0), got %.2f", a.TotalCost)
	}
	if a.Sessions != 1 {
		t.Errorf("scoped window should see only project A's in-window session, got %d", a.Sessions)
	}
	if len(a.Models) != 1 || a.Models[0].Label != "claude-opus-4-8" {
		t.Errorf("scoped window should hold only project A's in-window model: %+v", a.Models)
	}
	if len(a.Agents) != 1 || a.Agents[0].Label != "claude" {
		t.Errorf("scoped window should hold only project A's agent: %+v", a.Agents)
	}
}

// The unbounded (all-time) path derives its headline token totals from the daily
// series, which carries all four token classes. TestAnalyticsRollups leaves cache
// tokens at zero, so this pins the all-time cache and combined-token aggregation
// that the overview's Tokens readout and its tooltip surface.
func TestAnalyticsAllTimeTokenTotals(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	s1 := seedSessionWithStats(t, st, admin.ID, proj, "claude", "s1", 2.0, 0, 0)
	// Two dated events on different days, both carrying cache tokens.
	seedUsageCache(t, st, s1, "claude-opus-4-8", 1.0, 100, 20, 30, 7, 0, "c1")
	seedUsageCache(t, st, s1, "claude-opus-4-8", 1.0, 200, 40, 60, 14, 3, "c2")

	a, err := st.Analytics(ctx, proj, time.Time{}, nil)
	if err != nil {
		t.Fatalf("all-time analytics: %v", err)
	}
	if a.TotalIn != 300 || a.TotalOut != 60 {
		t.Errorf("all-time in/out wrong: in=%d out=%d, want 300/60", a.TotalIn, a.TotalOut)
	}
	if a.TotalCacheRead != 90 || a.TotalCacheWrite != 21 {
		t.Errorf("all-time cache totals wrong: read=%d write=%d, want 90/21", a.TotalCacheRead, a.TotalCacheWrite)
	}
	if got := a.TotalTokens(); got != 471 {
		t.Errorf("all-time combined tokens = %d, want 471 (300+60+90+21)", got)
	}
}

// The window's lower bound is inclusive: an event whose occurred_at is exactly
// `since` counts, while one a single instant earlier does not. The other store
// tests only use clearly inside/outside dates, leaving this edge unpinned.
func TestAnalyticsWindowLowerBoundInclusive(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	s1 := seedSessionWithStats(t, st, admin.ID, proj, "claude", "s1", 0, 0, 0)
	// Postgres timestamps are microsecond-resolution, so truncate to the same grid
	// and step by one microsecond to straddle the bound exactly.
	bound := time.Now().Add(-24 * time.Hour).Truncate(time.Microsecond)
	seedUsageAt(t, st, s1, "claude-opus-4-8", 1.0, 100, 20, bound, "at-bound")
	seedUsageAt(t, st, s1, "claude-opus-4-8", 5.0, 500, 90, bound.Add(-time.Microsecond), "below-bound")

	a, err := st.Analytics(ctx, proj, bound, nil)
	if err != nil {
		t.Fatalf("boundary analytics: %v", err)
	}
	if len(a.Series) != 1 {
		t.Errorf("only the at-bound event should land in the series, got %d points", len(a.Series))
	}
	if a.TotalCost < 0.99 || a.TotalCost > 1.01 {
		t.Errorf("inclusive bound should keep the at-bound event and drop the one below it (~1.0), got %.2f", a.TotalCost)
	}
	if a.TotalIn != 100 || a.TotalOut != 20 {
		t.Errorf("boundary token totals wrong: in=%d out=%d, want 100/20", a.TotalIn, a.TotalOut)
	}
}

// The windowed overview rollups bound usage by ue.occurred_at, so they need a
// supporting index or each bounded request seq-scans all accumulated history. This
// pins the partial index's presence (migration 0012): drop it and the windowed
// series, by-model, and by-agent rollups silently regress to full-table scans.
func TestUsageEventsOccurredAtIndex(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	var indexdef string
	err := st.Pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes
		  WHERE tablename = 'usage_events' AND indexname = 'idx_usage_events_occurred_at'`).
		Scan(&indexdef)
	if err != nil {
		t.Fatalf("the occurred_at index should exist to keep windowed rollups window-bound: %v", err)
	}
	// It must be the partial index on occurred_at, not some unrelated index that
	// happens to share the name: the lower bound seeks on occurred_at, and the
	// NULL-excluding predicate keeps undated events (never in any window) out.
	for _, want := range []string{"occurred_at", "WHERE", "IS NOT NULL"} {
		if !strings.Contains(indexdef, want) {
			t.Errorf("index def %q should mention %q (partial index on occurred_at)", indexdef, want)
		}
	}
}

func TestProjectSparklines(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	admin, _ := st.Register(ctx, "grace", "h", "")
	proj, _ := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	s1 := seedSessionWithStats(t, st, admin.ID, proj, "claude", "s1", 2.0, 100, 20)
	seedUsage(t, st, s1, "claude-opus-4-8", 1.0, 100, 20, 0, "a")
	seedUsage(t, st, s1, "claude-opus-4-8", 1.0, 100, 20, 5, "b")
	// Outside the 30-day window: must not appear in a 30-day sparkline.
	seedUsage(t, st, s1, "claude-opus-4-8", 9.0, 100, 20, 90, "old")

	spark, err := st.ProjectSparklines(ctx, 30)
	if err != nil {
		t.Fatalf("sparklines: %v", err)
	}
	vals, ok := spark[proj]
	if !ok {
		t.Fatal("project should have a sparkline")
	}
	if len(vals) != 30 {
		t.Fatalf("sparkline should be 30 days wide, got %d", len(vals))
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	// Only the two in-window events (1.0 + 1.0) count; the 90-days-ago event is excluded.
	if sum < 1.99 || sum > 2.01 {
		t.Errorf("sparkline should sum only in-window cost (~2.0), got %.2f", sum)
	}
}
