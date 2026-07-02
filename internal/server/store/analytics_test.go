package store_test

import (
	"context"
	"fmt"
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
	// message_count = 1 so the session is non-empty: the shared session-list conds
	// hide zero-message sessions by default, and these represent real sessions with
	// usage, not the empty ones the global feed suppresses.
	err := st.Pool.QueryRow(context.Background(),
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine,
		        message_count, total_cost_usd, total_input_tokens, total_output_tokens)
		 VALUES ($1,$2,$3,$4,'box',1,$5,$6,$7) RETURNING id`,
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

// seedUsageUndated inserts a usage event with a NULL occurred_at, the shape a
// transcript line with no timestamp produces. It has no place on the time axis, so
// the overview excludes it everywhere (headline, breakdowns, series, and the
// windowed view alike) to keep the headline equal to the chart.
func seedUsageUndated(t *testing.T, st *store.Store, sessionID int64, model string, cost float64, in, out int64, dedup string) {
	t.Helper()
	_, err := st.Pool.Exec(context.Background(),
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cost_usd, dedup_key)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		sessionID, model, in, out, cost, dedup)
	if err != nil {
		t.Fatalf("seed undated usage: %v", err)
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

// seedUsageUnpriced inserts a dated usage event that carries real token volume but
// a NULL cost, the shape an unpriced model produces. Its tokens count toward the
// totals while its cost does not, which is exactly what should flag an aggregate as
// cost-incomplete.
func seedUsageUnpriced(t *testing.T, st *store.Store, sessionID int64, model string, in, out int64, dedup string) {
	t.Helper()
	_, err := st.Pool.Exec(context.Background(),
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1,$2,$3,$4, NULL, now(), $5)`,
		sessionID, model, in, out, dedup)
	if err != nil {
		t.Fatalf("seed unpriced usage: %v", err)
	}
}

// TestCostIncompleteRollsUp pins that an unpriced usage event (tokens, no cost)
// propagates the "cost is a lower bound" marker into both aggregate paths the UI
// reads: the analytics headline and breakdowns, and the projects-index rollup.
// Without this, a project built partly from unpriced sessions would read an exact
// "$X" while its own session rows show "$X+".
func TestCostIncompleteRollsUp(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projA, err := st.UpsertProject(ctx, "github.com/ada/incomplete", "github.com", "ada", "incomplete", "incomplete", "remote")
	if err != nil {
		t.Fatalf("project A: %v", err)
	}
	projB, err := st.UpsertProject(ctx, "github.com/ada/priced", "github.com", "ada", "priced", "priced", "remote")
	if err != nil {
		t.Fatalf("project B: %v", err)
	}

	// Project A: one session whose usage mixes a priced event with an unpriced one.
	sA := seedSessionWithStats(t, st, admin.ID, projA, "claude", "a1", 1.0, 600, 120)
	seedUsage(t, st, sA, "claude-opus-4-8", 1.0, 500, 100, 0, "a-priced")
	seedUsageUnpriced(t, st, sA, "secret-model", 100, 20, "a-unpriced")

	// Project B: one session, fully priced.
	sB := seedSessionWithStats(t, st, admin.ID, projB, "claude", "b1", 2.0, 800, 160)
	seedUsage(t, st, sB, "claude-opus-4-8", 2.0, 800, 160, 0, "b-priced")

	// Analytics flags A's window as incomplete and at least one of its breakdown
	// rows; B's window is exact.
	aA, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: projA, Since: time.Time{}, UserIDs: nil})
	if err != nil {
		t.Fatalf("analytics A: %v", err)
	}
	if !aA.CostIncomplete {
		t.Error("project A analytics should be cost-incomplete (an unpriced usage event)")
	}
	var anyModelIncomplete bool
	for _, m := range aA.Models {
		if m.CostIncomplete {
			anyModelIncomplete = true
		}
	}
	if !anyModelIncomplete {
		t.Error("a by-model breakdown row should carry the incomplete marker")
	}
	aB, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: projB, Since: time.Time{}, UserIDs: nil})
	if err != nil {
		t.Fatalf("analytics B: %v", err)
	}
	if aB.CostIncomplete {
		t.Error("project B analytics is fully priced; should not be cost-incomplete")
	}

	// The projects index rolls bool_or(cost_incomplete) per project. Set A's session
	// flag the way projection would for an unpriced turn; leave B's clear.
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET cost_incomplete = TRUE WHERE id = $1", sA); err != nil {
		t.Fatalf("flag session: %v", err)
	}
	projects, err := st.ListProjects(ctx)
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	seen := map[int64]bool{}
	for _, p := range projects {
		seen[p.ID] = true
		switch p.ID {
		case projA:
			if !p.CostIncomplete {
				t.Error("project A index row should be cost-incomplete")
			}
		case projB:
			if p.CostIncomplete {
				t.Error("project B index row should not be cost-incomplete")
			}
		}
	}
	if !seen[projA] || !seen[projB] {
		t.Fatalf("both projects should appear in the index: %v", seen)
	}
}

// TestCostIncompleteReasoningOnly pins that reasoning tokens alone count as "real
// volume": a usage event with only reasoning_tokens and a NULL cost flags the
// analytics window incomplete, matching projection.go, which already folds
// reasoning into the session's cost_incomplete. Without reasoning in the analytics
// expression, a reasoning-only unpriced turn would read exact in the breakdown
// while the session rollup said otherwise.
func TestCostIncompleteReasoningOnly(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/reasoning", "github.com", "ada", "reasoning", "reasoning", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	s := seedSessionWithStats(t, st, admin.ID, proj, "claude", "r1", 0, 0, 0)
	// Only reasoning tokens, no cost: the other four classes are zero.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO usage_events (session_id, model, reasoning_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1,$2,$3, NULL, now(), $4)`,
		s, "secret-model", 500, "r-unpriced"); err != nil {
		t.Fatalf("seed reasoning-only usage: %v", err)
	}

	a, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj, Since: time.Time{}, UserIDs: nil})
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}
	if !a.CostIncomplete {
		t.Error("a reasoning-only unpriced usage event should flag the window cost-incomplete")
	}
}

// TestAnalyticsReasoningTokens pins the reasoning surfacing: the window total and the
// per-agent split carry the reasoning-token class, and it stays OUT of the four-class
// Tokens total so the headline-equals-sum reconciliation the billed classes hold is
// undisturbed.
func TestAnalyticsReasoningTokens(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	user, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/reason", "github.com", "ada", "reason", "reason", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	// A codex session reports reasoning tokens; a claude session reports none.
	sX := seedSessionWithStats(t, st, user.ID, proj, "codex", "x1", 1.0, 100, 200)
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, reasoning_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1,'gpt-5.5',100,200,500,1.0, now(), 'x-u')`, sX); err != nil {
		t.Fatalf("seed codex usage: %v", err)
	}
	sC := seedSessionWithStats(t, st, user.ID, proj, "claude", "c1", 0.5, 300, 50)
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, reasoning_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1,'claude-opus-4-8',300,50,0,0.5, now(), 'c-u')`, sC); err != nil {
		t.Fatalf("seed claude usage: %v", err)
	}

	a, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj})
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}
	if a.TotalReasoning != 500 {
		t.Errorf("TotalReasoning = %d, want 500", a.TotalReasoning)
	}
	// The four billed classes total 100+200+300+50 = 650; reasoning stays out of it.
	if a.TotalTokens() != 650 {
		t.Errorf("TotalTokens = %d, want 650 (reasoning must not fold into the billed classes)", a.TotalTokens())
	}
	byAgent := map[string]store.Breakdown{}
	for _, ag := range a.Agents {
		byAgent[ag.Label] = ag
	}
	if byAgent["codex"].Reasoning != 500 {
		t.Errorf("codex reasoning = %d, want 500", byAgent["codex"].Reasoning)
	}
	if byAgent["claude"].Reasoning != 0 {
		t.Errorf("claude reasoning = %d, want 0", byAgent["claude"].Reasoning)
	}
	// The by-model split must sum to the same reasoning total as the by-agent split, the
	// same three-way reconciliation the billed classes hold.
	var modelReasoning int64
	for _, m := range a.Models {
		modelReasoning += m.Reasoning
	}
	if modelReasoning != a.TotalReasoning {
		t.Errorf("by-model reasoning sum = %d, want %d (must match the headline)", modelReasoning, a.TotalReasoning)
	}
}

// TestAnalyticsFiltersByAgentAndMachine pins the project page's reconciliation fix:
// the usage panel scopes to the same agent/machine the session table does, so a
// filtered headline reflects only the filtered slice rather than staying
// project-wide. Without this, /projects/<id>?agent=claude would narrow the rows
// while the headline still summed every agent.
func TestAnalyticsFiltersByAgentAndMachine(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	user, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/multi", "github.com", "ada", "multi", "multi", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	sC := seedSessionWithStats(t, st, user.ID, proj, "claude", "c1", 1.0, 500, 100)
	seedUsage(t, st, sC, "claude-opus-4-8", 1.0, 500, 100, 0, "c-u")
	sX := seedSessionWithStats(t, st, user.ID, proj, "codex", "x1", 2.0, 800, 200)
	seedUsage(t, st, sX, "gpt-5.5", 2.0, 800, 200, 0, "x-u")

	all, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj})
	if err != nil {
		t.Fatalf("analytics all: %v", err)
	}
	if all.TotalIn != 1300 || all.Sessions != 2 {
		t.Errorf("unfiltered = in %d sessions %d, want 1300/2", all.TotalIn, all.Sessions)
	}

	claude, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj, Agent: "claude"})
	if err != nil {
		t.Fatalf("analytics agent: %v", err)
	}
	if claude.TotalIn != 500 || claude.TotalOut != 100 || claude.Sessions != 1 {
		t.Errorf("agent=claude = in %d out %d sessions %d, want 500/100/1", claude.TotalIn, claude.TotalOut, claude.Sessions)
	}
	if len(claude.Agents) != 1 || claude.Agents[0].Label != "claude" {
		t.Errorf("agent=claude by-agent split = %+v, want one claude row", claude.Agents)
	}

	// Machine narrows identically. Give the claude session a distinct machine, then
	// scope to it: only that session's usage should remain.
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET machine = 'laptop' WHERE id = $1", sC); err != nil {
		t.Fatalf("set machine: %v", err)
	}
	lap, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj, Machine: "laptop"})
	if err != nil {
		t.Fatalf("analytics machine: %v", err)
	}
	if lap.TotalIn != 500 || lap.Sessions != 1 {
		t.Errorf("machine=laptop = in %d sessions %d, want 500/1", lap.TotalIn, lap.Sessions)
	}

	// Username scopes by account name, the form the project page's filter carries.
	grace, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj, Username: "grace"})
	if err != nil {
		t.Fatalf("analytics username: %v", err)
	}
	if grace.Sessions != 2 || grace.TotalIn != 1300 {
		t.Errorf("user=grace = in %d sessions %d, want 1300/2", grace.TotalIn, grace.Sessions)
	}
	// An unknown name must scope to nothing, not fall back to every user: that empty
	// result is what keeps the panel in lockstep with the session list's empty table.
	none, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj, Username: "ghost"})
	if err != nil {
		t.Fatalf("analytics unknown username: %v", err)
	}
	if none.Sessions != 0 || none.TotalIn != 0 || none.HasData() {
		t.Errorf("user=ghost should scope to nothing, got in %d sessions %d", none.TotalIn, none.Sessions)
	}
}

// TestWindowSessionsPartitionPanel pins the project page's row/panel reconciliation:
// WindowSessionPage returns each session's in-window token share, so the visible rows
// are a partition of the usage panel (they sum to its headline and match its session
// tally) even under a narrow window, where the lifetime rollups would overcount a
// session whose usage predates the window.
func TestWindowSessionsPartitionPanel(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	user, err := st.Register(ctx, "grace", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/window", "github.com", "ada", "window", "window", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	now := time.Now()
	recent := now.Add(-24 * time.Hour)
	old := now.Add(-40 * 24 * time.Hour)

	// Session A spans the window boundary: 100/50 recent, 1000/500 well before it.
	sA := seedSessionWithStats(t, st, user.ID, proj, "claude", "a1", 0, 0, 0)
	seedUsageAt(t, st, sA, "claude-opus-4-8", 1.0, 100, 50, recent, "a-recent")
	seedUsageAt(t, st, sA, "claude-opus-4-8", 5.0, 1000, 500, old, "a-old")
	// Session B is entirely before the window.
	sB := seedSessionWithStats(t, st, user.ID, proj, "claude", "b1", 0, 0, 0)
	seedUsageAt(t, st, sB, "claude-opus-4-8", 2.0, 200, 100, old, "b-old")

	since := now.Add(-30 * 24 * time.Hour)
	page, err := st.WindowSessionPage(ctx, store.SessionFilter{ProjectID: proj, Since: since})
	if err != nil {
		t.Fatalf("window sessions: %v", err)
	}
	win := page.Sessions
	a, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj, Since: since})
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}

	// Under the 30-day window only A's recent usage qualifies: one row, its tokens
	// the in-window share (100/50), not the 1100/550 lifetime sum.
	if len(win) != 1 || win[0].ID != sA {
		t.Fatalf("window rows = %d, want exactly session A", len(win))
	}
	if win[0].TotalInput != 100 || win[0].TotalOutput != 50 {
		t.Errorf("windowed row tokens = in %d out %d, want in-window 100/50", win[0].TotalInput, win[0].TotalOutput)
	}
	var sumIn int64
	for _, s := range win {
		sumIn += s.TotalInput
	}
	if sumIn != a.TotalIn {
		t.Errorf("sum of row input %d != panel headline input %d", sumIn, a.TotalIn)
	}
	if len(win) != a.Sessions {
		t.Errorf("row count %d != panel session tally %d", len(win), a.Sessions)
	}
	// Well under the cap, so no tail is withheld and the footer stays empty.
	if page.Remainder.Has() {
		t.Errorf("remainder should be empty under the cap, got %+v", page.Remainder)
	}

	// Widening to all of history brings B in and restores A's older usage; the rows
	// still partition the panel.
	fullPage, err := st.WindowSessionPage(ctx, store.SessionFilter{ProjectID: proj})
	if err != nil {
		t.Fatalf("window all: %v", err)
	}
	full := fullPage.Sessions
	fa, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj})
	if err != nil {
		t.Fatalf("analytics all: %v", err)
	}
	var fullIn int64
	for _, s := range full {
		fullIn += s.TotalInput
	}
	if len(full) != 2 || fullIn != fa.TotalIn || fullIn != 1300 {
		t.Errorf("all-history rows = %d sumIn %d (panel %d), want 2 rows summing to 1300", len(full), fullIn, fa.TotalIn)
	}
}

// TestWindowSessionsCapEngages pins the cap and the remainder that closes the gap it
// opens. With more matching sessions than the cap, WindowSessionPage returns exactly
// the cap of rows while Analytics still sums the whole windowed base, so the rows
// alone fall short of the headline by a real tail. The remainder must reconcile that
// tail exactly: per token class and cost, the shown rows plus the remainder reproduce
// the panel. It also pins the projection-consistency counterexample: when a visible
// row is the unpriced one and the hidden tail is fully priced, the panel is correctly
// cost-incomplete but the remainder must not be, because its flag is a bool_or over
// the hidden sessions alone, not a copy of the panel's.
func TestWindowSessionsCapEngages(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	user, err := st.Register(ctx, "ada", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/cap", "github.com", "ada", "cap", "cap", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	// 101 sessions, all with recent dated usage so they fall inside the window and the
	// cap of 100 withholds exactly one: the first inserted (lowest id, oldest
	// updated_at), which the newest-active ordering pushes past the cap. Every session
	// is priced except a visible one (the last inserted), so the panel reads
	// cost-incomplete from a row that shows, while the hidden tail is fully priced.
	const total = 101
	now := time.Now()
	var hiddenID int64
	for i := 0; i < total; i++ {
		sid := seedSessionWithStats(t, st, user.ID, proj, "claude", fmt.Sprintf("s%d", i), 0, 0, 0)
		if i == 0 {
			hiddenID = sid
		}
		if i == total-1 {
			// The newest session shows, and it carries the unpriced usage, so the panel
			// flag comes from a visible row, not the hidden tail.
			seedUsageUnpriced(t, st, sid, "mystery-model", 10, 5, fmt.Sprintf("u%d", i))
			continue
		}
		seedUsageAt(t, st, sid, "claude-opus-4-8", 1.0, 10, 5, now.Add(-time.Duration(i+1)*time.Hour), fmt.Sprintf("u%d", i))
	}

	since := now.Add(-30 * 24 * time.Hour)
	page, err := st.WindowSessionPage(ctx, store.SessionFilter{ProjectID: proj, Since: since})
	if err != nil {
		t.Fatalf("window sessions: %v", err)
	}
	a, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj, Since: since})
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}

	if len(page.Sessions) != 100 {
		t.Fatalf("windowed rows = %d, want the cap of 100", len(page.Sessions))
	}
	if a.Sessions != total {
		t.Fatalf("panel session tally = %d, want all %d", a.Sessions, total)
	}
	for _, s := range page.Sessions {
		if s.ID == hiddenID {
			t.Fatalf("session %d should have been withheld past the cap but is shown", hiddenID)
		}
	}

	// The remainder is exactly the one withheld session.
	rem := page.Remainder
	if rem.Sessions != 1 {
		t.Fatalf("remainder sessions = %d, want 1", rem.Sessions)
	}

	// Shown rows plus the remainder reproduce the panel, per token class and cost.
	var shownIn, shownOut, shownCR, shownCW int64
	var shownCost float64
	for _, s := range page.Sessions {
		shownIn += s.TotalInput
		shownOut += s.TotalOutput
		shownCR += s.TotalCacheRead
		shownCW += s.TotalCacheWrite
		shownCost += s.TotalCostUSD
	}
	if shownIn+rem.Input != a.TotalIn || shownOut+rem.Output != a.TotalOut ||
		shownCR+rem.CacheRead != a.TotalCacheRead || shownCW+rem.CacheWrite != a.TotalCacheWrite {
		t.Errorf("shown+remainder tokens (%d/%d/%d/%d + %d/%d/%d/%d) != panel (%d/%d/%d/%d)",
			shownIn, shownOut, shownCR, shownCW, rem.Input, rem.Output, rem.CacheRead, rem.CacheWrite,
			a.TotalIn, a.TotalOut, a.TotalCacheRead, a.TotalCacheWrite)
	}
	if shownCost+rem.CostUSD != a.TotalCost {
		t.Errorf("shown+remainder cost %.2f != panel %.2f", shownCost+rem.CostUSD, a.TotalCost)
	}

	// The panel is cost-incomplete (a visible row is unpriced), but the hidden tail is
	// fully priced, so the footer must not inherit the panel's marker.
	if !a.CostIncomplete {
		t.Error("panel should be cost-incomplete: a shown session carries unpriced usage")
	}
	if rem.CostIncomplete {
		t.Error("remainder must not be cost-incomplete: the hidden session is fully priced")
	}
}

// TestWindowSessionRemainderFlagsHiddenTail is the other direction of the bool_or: a
// hidden session carries the unpriced usage while every shown row is priced. The
// remainder must flag itself cost-incomplete so the footer marks its hidden cost a
// lower bound, proving the marker tracks the hidden sessions rather than copying the
// panel (which here happens to agree, but for the right reason).
func TestWindowSessionRemainderFlagsHiddenTail(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	user, err := st.Register(ctx, "ada", "h", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/ada/tail", "github.com", "ada", "tail", "tail", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	const total = 101
	now := time.Now()
	for i := 0; i < total; i++ {
		sid := seedSessionWithStats(t, st, user.ID, proj, "claude", fmt.Sprintf("s%d", i), 0, 0, 0)
		if i == 0 {
			// The first inserted is the one withheld past the cap; give it the only
			// unpriced usage so the incompleteness lives entirely in the hidden tail.
			seedUsageUnpriced(t, st, sid, "mystery-model", 10, 5, fmt.Sprintf("u%d", i))
			continue
		}
		seedUsageAt(t, st, sid, "claude-opus-4-8", 1.0, 10, 5, now.Add(-time.Duration(i+1)*time.Hour), fmt.Sprintf("u%d", i))
	}

	since := now.Add(-30 * 24 * time.Hour)
	page, err := st.WindowSessionPage(ctx, store.SessionFilter{ProjectID: proj, Since: since})
	if err != nil {
		t.Fatalf("window sessions: %v", err)
	}
	if page.Remainder.Sessions != 1 {
		t.Fatalf("remainder sessions = %d, want 1 withheld", page.Remainder.Sessions)
	}
	if !page.Remainder.CostIncomplete {
		t.Error("remainder must be cost-incomplete: the hidden session carries unpriced usage")
	}
	// No shown row is unpriced, so the table's own rows read exact; the footer is the
	// only place the lower-bound marker appears.
	for _, s := range page.Sessions {
		if s.CostIncomplete {
			t.Errorf("shown session %d should be fully priced", s.ID)
		}
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

	a, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj, Since: time.Time{}, UserIDs: nil})
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
	// Totals sum the usage events the breakdowns roll up: 3.0 + 1.0.
	if a.TotalCost < 3.99 || a.TotalCost > 4.01 {
		t.Errorf("total cost should sum the usage events to ~4.0, got %.2f", a.TotalCost)
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
	g, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: 0, Since: time.Time{}, UserIDs: nil})
	if err != nil {
		t.Fatalf("global analytics: %v", err)
	}
	if g.Sessions != 2 || len(g.Series) != 3 {
		t.Errorf("global rollup mismatch: %+v", g)
	}
}

// A non-empty userIDs scopes every rollup to the named users' sessions, leaving
// other users' usage out of the series, the breakdowns, and the totals. It
// exercises both the unbounded and the windowed views (the same usage_events
// base, with and without a time bound), and confirms an empty selection is the
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
	g, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: 0, Since: time.Time{}, UserIDs: []int64{graceID}})
	if err != nil {
		t.Fatalf("grace all-time analytics: %v", err)
	}
	if g.Sessions != 1 {
		t.Errorf("grace scope should see only her session, got %d", g.Sessions)
	}
	if g.TotalCost < 2.99 || g.TotalCost > 3.01 {
		t.Errorf("grace scope cost should sum her usage to ~3.0, got %.2f", g.TotalCost)
	}
	if len(g.Agents) != 1 || g.Agents[0].Label != "claude" {
		t.Errorf("grace scope agents should hold only claude: %+v", g.Agents)
	}
	if len(g.Models) != 1 || g.Models[0].Label != "claude-opus-4-8" {
		t.Errorf("grace scope models should hold only her model: %+v", g.Models)
	}

	// Scoped to grace, windowed: the usage_events path agrees with the rollup path.
	since := time.Now().AddDate(0, 0, -7)
	gw, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: 0, Since: since, UserIDs: []int64{graceID}})
	if err != nil {
		t.Fatalf("grace windowed analytics: %v", err)
	}
	if gw.Sessions != 1 || gw.TotalIn != 900 || gw.TotalOut != 180 {
		t.Errorf("grace windowed scope wrong: sessions=%d in=%d out=%d", gw.Sessions, gw.TotalIn, gw.TotalOut)
	}

	// Both users selected matches the unscoped view: two sessions, full spend.
	both, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: 0, Since: time.Time{}, UserIDs: []int64{graceID, adaID}})
	if err != nil {
		t.Fatalf("both-user analytics: %v", err)
	}
	all, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: 0, Since: time.Time{}, UserIDs: nil})
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
	w, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: projA, Since: since, UserIDs: []int64{graceID}})
	if err != nil {
		t.Fatalf("windowed combined analytics: %v", err)
	}
	assertGraceProjA("windowed", w)
	if w.TotalIn != 200 || w.TotalOut != 40 {
		t.Errorf("windowed combined token totals wrong: in=%d out=%d, want 200/40", w.TotalIn, w.TotalOut)
	}

	// Unbounded view (no time bound): project A AND grace.
	all, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: projA, Since: time.Time{}, UserIDs: []int64{graceID}})
	if err != nil {
		t.Fatalf("all-time combined analytics: %v", err)
	}
	assertGraceProjA("all-time", all)
}

// analyticsByUser groups the same filtered usage base as the by-model and by-agent
// splits by the session's owning user, ordered by cost desc like the other two. This
// mirrors TestAnalyticsRollups' shape for the by-agent split, but keyed on the user
// dimension instead.
func TestAnalyticsByUser(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	graceID := seedUser(t, st, "grace")
	adaID := seedUser(t, st, "ada")
	proj, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	// grace: two sessions, higher total cost. ada: one session, lower cost.
	sg1 := seedSessionWithStats(t, st, graceID, proj, "claude", "sg1", 3.0, 500, 100)
	sg2 := seedSessionWithStats(t, st, graceID, proj, "codex", "sg2", 2.0, 300, 60)
	sa := seedSessionWithStats(t, st, adaID, proj, "claude", "sa1", 1.0, 200, 40)
	seedUsage(t, st, sg1, "claude-opus-4-8", 3.0, 500, 100, 0, "g1")
	seedUsage(t, st, sg2, "gpt-5.5", 2.0, 300, 60, 0, "g2")
	seedUsage(t, st, sa, "claude-opus-4-8", 1.0, 200, 40, 0, "a1")

	a, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj})
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}

	if len(a.Users) != 2 {
		t.Fatalf("want 2 users in the breakdown, got %d: %+v", len(a.Users), a.Users)
	}
	// Ordered by cost desc: grace (5.0 across two sessions) leads ada (1.0).
	if a.Users[0].Label != "grace" || a.Users[1].Label != "ada" {
		t.Errorf("users breakdown should lead with grace (higher cost): %+v", a.Users)
	}
	if !costsEqual(a.Users[0].CostUSD, 5.0) {
		t.Errorf("grace's cost = %.2f, want ~5.0 (both her sessions)", a.Users[0].CostUSD)
	}
	if a.Users[0].Sessions != 2 {
		t.Errorf("grace's session count = %d, want 2 (grouping by user, not by agent)", a.Users[0].Sessions)
	}
	if !costsEqual(a.Users[1].CostUSD, 1.0) || a.Users[1].Sessions != 1 {
		t.Errorf("ada's row = cost %.2f sessions %d, want 1.0/1", a.Users[1].CostUSD, a.Users[1].Sessions)
	}
	if a.Users[0].Input != 800 {
		t.Errorf("grace's input tokens = %d, want 800 (500+300, summed across her two sessions)", a.Users[0].Input)
	}

	// The by-user split reconciles with the headline the same way by-model and
	// by-agent do: it partitions the same usage_events base one user at a time.
	var userTok int64
	var userCost float64
	for _, u := range a.Users {
		userTok += u.Tokens()
		userCost += u.CostUSD
	}
	if userTok != a.TotalTokens() {
		t.Errorf("by-user token sum %d != headline %d", userTok, a.TotalTokens())
	}
	if !costsEqual(userCost, a.TotalCost) {
		t.Errorf("by-user cost sum %.2f != headline %.2f", userCost, a.TotalCost)
	}

	// OmitUsers drops the by-user split (the public project overview sets it, since its
	// panel never renders Users) while every headline total stays intact: the totals sum
	// from the by-agent split, not the by-user one, so omitting the latter changes no
	// figure. The by-model and by-agent splits the public panel does render are untouched.
	omit, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj, OmitUsers: true})
	if err != nil {
		t.Fatalf("analytics OmitUsers: %v", err)
	}
	if len(omit.Users) != 0 {
		t.Errorf("OmitUsers should drop the by-user split, got %d rows: %+v", len(omit.Users), omit.Users)
	}
	if omit.TotalTokens() != a.TotalTokens() || !costsEqual(omit.TotalCost, a.TotalCost) || omit.Sessions != a.Sessions {
		t.Errorf("OmitUsers changed the headline: tokens %d/%d cost %.2f/%.2f sessions %d/%d",
			omit.TotalTokens(), a.TotalTokens(), omit.TotalCost, a.TotalCost, omit.Sessions, a.Sessions)
	}
	if len(omit.Models) != len(a.Models) || len(omit.Agents) != len(a.Agents) {
		t.Errorf("OmitUsers should not touch the model/agent splits: models %d/%d agents %d/%d",
			len(omit.Models), len(a.Models), len(omit.Agents), len(a.Agents))
	}
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

// The headline equals the sum of the rows beneath it, in every shape: the
// by-model split, the by-agent split, and the daily series all add up to the
// headline. This is the property the whole single-source design exists to hold,
// with no figure adding up to a different number than the chart or the bars under
// it. The mix is chosen to break a naive implementation: an unpriced event with
// an empty model id (the old by-model query dropped every empty-model row), an
// undated event (which has no day to plot, so it must drop from the headline too,
// not just the chart), and cache tokens that dwarf in/out (the gap that started
// this).
func TestAnalyticsHeadlineMatchesBreakdowns(t *testing.T) {
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

	sClaude := seedSessionWithStats(t, st, admin.ID, proj, "claude", "scl", 0, 0, 0)
	sCodex := seedSessionWithStats(t, st, admin.ID, proj, "codex", "scx", 0, 0, 0)

	// Cache-dominant priced usage on both agents, in-window.
	seedUsageCache(t, st, sClaude, "claude-opus-4-8", 2.0, 1000, 200, 5000, 300, 1, "lc1")
	seedUsageCache(t, st, sCodex, "gpt-5.5", 3.0, 800, 400, 9000, 0, 2, "lx1")
	// An unpriced event with no model id: it must still land in the totals and fold
	// into the by-model split rather than vanish.
	seedUsage(t, st, sCodex, "", 0, 200, 20, 1, "lx2")
	// An undated event: with no day to plot it cannot sit in the daily series, so
	// to keep the headline equal to the chart it must drop from every overview view.
	seedUsageUndated(t, st, sClaude, "claude-opus-4-8", 0.5, 100, 50, "lc2")

	sumBreakdown := func(bs []store.Breakdown) (tokens int64, cost float64) {
		for _, b := range bs {
			tokens += b.Tokens()
			cost += b.CostUSD
		}
		return
	}
	sumSeries := func(ps []store.DayPoint) (tokens int64, cost float64) {
		for _, p := range ps {
			tokens += p.Input + p.Output + p.CacheRead + p.CacheWrite
			cost += p.CostUSD
		}
		return
	}
	assertLinesUp := func(label string, a store.Analytics) {
		t.Helper()
		mTok, mCost := sumBreakdown(a.Models)
		gTok, gCost := sumBreakdown(a.Agents)
		sTok, sCost := sumSeries(a.Series)
		for _, c := range []struct {
			name string
			tok  int64
			cost float64
		}{{"by-model", mTok, mCost}, {"by-agent", gTok, gCost}, {"daily series", sTok, sCost}} {
			if a.TotalTokens() != c.tok {
				t.Errorf("%s: headline tokens %d != sum of %s %d", label, a.TotalTokens(), c.name, c.tok)
			}
			if !costsEqual(a.TotalCost, c.cost) {
				t.Errorf("%s: headline cost %.4f != sum of %s %.4f", label, a.TotalCost, c.name, c.cost)
			}
		}
	}

	// All-time: every dated event counts, including the unpriced one; the undated
	// event does not, so the headline still equals the chart.
	all, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj, Since: time.Time{}, UserIDs: nil})
	if err != nil {
		t.Fatalf("all-time analytics: %v", err)
	}
	assertLinesUp("all-time", all)
	// Pin the absolute totals so a sign or class error cannot pass by merely being
	// self-consistent: 1000+800+200 in, 200+400+20 out, 5000+9000 cache read, 300
	// cache write. The undated 150 tokens / $0.50 are excluded.
	if all.TotalTokens() != 16920 {
		t.Errorf("all-time total tokens = %d, want 16920 (the undated 150 excluded)", all.TotalTokens())
	}
	if !costsEqual(all.TotalCost, 5.0) {
		t.Errorf("all-time total cost = %.4f, want 5.0 (the undated 0.50 excluded)", all.TotalCost)
	}

	// Windowed: all dated events fall inside a 7-day window, so the window sees the
	// same dated set and reconciles identically.
	since := time.Now().AddDate(0, 0, -7)
	win, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj, Since: since, UserIDs: nil})
	if err != nil {
		t.Fatalf("windowed analytics: %v", err)
	}
	assertLinesUp("windowed", win)
	if win.TotalTokens() != 16920 {
		t.Errorf("windowed total tokens = %d, want 16920", win.TotalTokens())
	}
	if !costsEqual(win.TotalCost, 5.0) {
		t.Errorf("windowed total cost = %.4f, want 5.0", win.TotalCost)
	}
}

// costsEqual reports whether two USD sums are within a hundredth of a cent, the slack a
// float sum of priced events needs.
func costsEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 0.0001
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
	a, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: 0, Since: since, UserIDs: nil})
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
	full, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: 0, Since: time.Time{}, UserIDs: nil})
	if err != nil {
		t.Fatalf("full analytics: %v", err)
	}
	if full.Sessions != 2 {
		t.Errorf("unbounded view should see both sessions, got %d", full.Sessions)
	}
	if full.TotalCost < 10.99 || full.TotalCost > 11.01 {
		t.Errorf("unbounded cost should sum all usage to ~11.0, got %.2f", full.TotalCost)
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
	a, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: projA, Since: since, UserIDs: nil})
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

// The all-time headline token totals sum the by-agent split over usage_events and
// carry all four token classes. The session rollup here is seeded at zero tokens
// on purpose: the totals must come from the usage events, not the rollup column,
// so a stale or unbacked rollup cannot skew the readout. TestAnalyticsRollups
// leaves cache tokens at zero, so this pins the all-time cache and combined-token
// aggregation that the overview's Tokens readout and its tooltip surface.
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

	a, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj, Since: time.Time{}, UserIDs: nil})
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

	a, err := st.Analytics(ctx, store.AnalyticsFilter{ProjectID: proj, Since: bound, UserIDs: nil})
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
