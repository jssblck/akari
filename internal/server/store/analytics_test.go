package store

import (
	"context"
	"testing"
	"time"
)

// seedUsage inserts a session and a usage event directly, bypassing the ingest
// pipeline, so the analytics rollups can be asserted against known inputs.
func seedSessionWithStats(t *testing.T, st *Store, userID, projectID int64, agent, src string, cost float64, in, out int64) int64 {
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

func seedUsage(t *testing.T, st *Store, sessionID int64, model string, cost float64, in, out int64, daysAgo int, dedup string) {
	t.Helper()
	_, err := st.Pool.Exec(context.Background(),
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1,$2,$3,$4,$5, now() - make_interval(days => $6), $7)`,
		sessionID, model, in, out, cost, daysAgo, dedup)
	if err != nil {
		t.Fatalf("seed usage: %v", err)
	}
}

func TestAnalyticsRollups(t *testing.T) {
	st := newTestStore(t)
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

	a, err := st.Analytics(ctx, proj, time.Time{})
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
	g, err := st.Analytics(ctx, 0, time.Time{})
	if err != nil {
		t.Fatalf("global analytics: %v", err)
	}
	if g.Sessions != 2 || len(g.Series) != 3 {
		t.Errorf("global rollup mismatch: %+v", g)
	}
}

// A non-zero `since` bounds every rollup to the trailing window, slicing usage by
// event time. Only events at or after the bound count toward the series, the
// breakdowns, the totals, and the distinct-session count.
func TestAnalyticsTimeWindow(t *testing.T) {
	st := newTestStore(t)
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
	a, err := st.Analytics(ctx, 0, since)
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
	full, err := st.Analytics(ctx, 0, time.Time{})
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

func TestProjectSparklines(t *testing.T) {
	st := newTestStore(t)
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
