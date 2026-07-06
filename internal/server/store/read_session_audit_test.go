package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestSubagentsCarryVerdicts pins the outcome-aware subagents read: each direct child
// comes back with its current outcome and grade (nil/empty while unsettled or stale),
// its first-prompt title, and Failed() counting only errored children, so the fold
// summary's "N failed" and the audit header's waste note read the same set the child's
// own page shows.
func TestSubagentsCarryVerdicts(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uid := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatal(err)
	}
	announce := func(src string) int64 {
		t.Helper()
		ann, err := st.Announce(ctx, store.AnnounceParams{UserID: uid, Agent: "claude", SourceSessionID: src, ProjectID: pid})
		if err != nil {
			t.Fatalf("announce %q: %v", src, err)
		}
		return ann.SessionID
	}
	parent := announce("parent-uuid")
	errored := announce("parent-uuid/subagents/agent-err")
	completed := announce("parent-uuid/subagents/agent-ok")
	unsettled := announce("parent-uuid/subagents/agent-live")

	// The errored child carries a scored, fresh signals row; the completed one a
	// fresh row too; the third stays unsettled (no row at all).
	for _, row := range []struct {
		id      int64
		outcome string
		score   int
		grade   string
	}{
		{errored, "errored", 30, "F"},
		{completed, "completed", 100, "A"},
	} {
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO session_signals (session_id, outcome, outcome_confidence, score, grade)
			 VALUES ($1, $2, 'high', $3, $4)`, row.id, row.outcome, row.score, row.grade); err != nil {
			t.Fatalf("signals for %d: %v", row.id, err)
		}
		if _, err := st.Pool.Exec(ctx,
			`UPDATE sessions SET signals_stale = false WHERE id = $1`, row.id); err != nil {
			t.Fatalf("freshen %d: %v", row.id, err)
		}
	}
	// The errored child has a first prompt, so the table can name its task.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO messages (session_id, ordinal, role, content) VALUES ($1, 0, 'user', 'Verify the payment flow end to end.')`,
		errored); err != nil {
		t.Fatalf("child prompt: %v", err)
	}

	subs, err := st.Subagents(ctx, parent)
	if err != nil {
		t.Fatalf("subagents: %v", err)
	}
	if len(subs) != 3 {
		t.Fatalf("got %d subagents, want 3", len(subs))
	}
	byID := map[int64]store.SubagentRow{}
	for _, s := range subs {
		byID[s.ID] = s
	}

	e := byID[errored]
	if e.Outcome != "errored" || e.Grade == nil || *e.Grade != "F" || !e.Failed() {
		t.Fatalf("errored child = outcome %q grade %v failed %v", e.Outcome, e.Grade, e.Failed())
	}
	if e.Title != "Verify the payment flow end to end." {
		t.Fatalf("errored child title = %q", e.Title)
	}
	c := byID[completed]
	if c.Outcome != "completed" || c.Grade == nil || *c.Grade != "A" || c.Failed() {
		t.Fatalf("completed child = outcome %q grade %v failed %v", c.Outcome, c.Grade, c.Failed())
	}
	u := byID[unsettled]
	if u.Outcome != "" || u.Grade != nil || u.Failed() {
		t.Fatalf("unsettled child should carry no verdict, got outcome %q grade %v", u.Outcome, u.Grade)
	}
}

// TestSubagentVerdictIgnoresStaleSignals pins the freshness gate: a child whose
// projection moved after its grade (signals_stale set) reads as unmeasured here, the
// same signalsCurrent() rule every fleet read applies, so the fold summary never counts
// a failure the child's own page would not show.
func TestSubagentVerdictIgnoresStaleSignals(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uid := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{UserID: uid, Agent: "claude", SourceSessionID: "p", ProjectID: pid})
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.Announce(ctx, store.AnnounceParams{UserID: uid, Agent: "claude", SourceSessionID: "p/subagents/a", ProjectID: pid})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals (session_id, outcome, outcome_confidence, score, grade)
		 VALUES ($1, 'errored', 'high', 30, 'F')`, child.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE sessions SET signals_stale = true WHERE id = $1`, child.SessionID); err != nil {
		t.Fatal(err)
	}

	subs, err := st.Subagents(ctx, ann.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || subs[0].Outcome != "" || subs[0].Grade != nil || subs[0].Failed() {
		t.Fatalf("stale signals must read as unmeasured, got %+v", subs)
	}
}

// TestTreeRollupFor pins the single-session wrapper over the feed's recursive rollup:
// the parent's figure folds its own cost plus its whole descendant subtree, and a
// session that spawned nothing reads the zero rollup.
func TestTreeRollupFor(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uid := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatal(err)
	}
	announce := func(src string) int64 {
		t.Helper()
		ann, err := st.Announce(ctx, store.AnnounceParams{UserID: uid, Agent: "claude", SourceSessionID: src, ProjectID: pid})
		if err != nil {
			t.Fatalf("announce %q: %v", src, err)
		}
		return ann.SessionID
	}
	parent := announce("root")
	childA := announce("root/subagents/a")
	childB := announce("root/subagents/b")
	for id, cost := range map[int64]float64{parent: 1.00, childA: 0.25, childB: 0.50} {
		if _, err := st.Pool.Exec(ctx,
			`UPDATE sessions SET total_cost_usd = $2 WHERE id = $1`, id, cost); err != nil {
			t.Fatal(err)
		}
	}

	roll, err := st.TreeRollupFor(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if roll.SubagentCount != 2 || roll.CostUSD != 1.75 {
		t.Fatalf("rollup = %+v, want 2 subagents at $1.75", roll)
	}

	leaf, err := st.TreeRollupFor(ctx, childA)
	if err != nil {
		t.Fatal(err)
	}
	if leaf.SubagentCount != 0 {
		t.Fatalf("a leaf session should roll up no subagents, got %+v", leaf)
	}
}

// TestSessionAuditByID pins the one-snapshot audit bundle: the detail, signals,
// subagents, work-item rollup, and models the header judges side by side all arrive
// from a single read and agree with the individual reads on quiet data. (The snapshot
// property itself is the repeatable-read transaction; this pins the wiring.)
func TestSessionAuditByID(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uid := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatal(err)
	}
	parent, err := st.Announce(ctx, store.AnnounceParams{UserID: uid, Agent: "claude", SourceSessionID: "audit-root", ProjectID: pid})
	if err != nil {
		t.Fatal(err)
	}
	child, err := st.Announce(ctx, store.AnnounceParams{UserID: uid, Agent: "claude", SourceSessionID: "audit-root/subagents/a", ProjectID: pid})
	if err != nil {
		t.Fatal(err)
	}
	for id, cost := range map[int64]float64{parent.SessionID: 2.00, child.SessionID: 0.50} {
		if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET total_cost_usd = $2 WHERE id = $1`, id, cost); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_signals (session_id, outcome, outcome_confidence, score, grade)
		 VALUES ($1, 'completed', 'high', 95, 'A')`, parent.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET signals_stale = false WHERE id = $1`, parent.SessionID); err != nil {
		t.Fatal(err)
	}
	seedUsage(t, st, parent.SessionID, "claude-fable-5", 1.0, 1000, 100, 0, "audit-u1")

	a, err := st.SessionAuditByID(ctx, parent.SessionID)
	if err != nil {
		t.Fatalf("audit bundle: %v", err)
	}
	if a.Detail.ID != parent.SessionID || a.Detail.TotalCostUSD != 2.00 {
		t.Fatalf("bundle detail = id %d cost %v", a.Detail.ID, a.Detail.TotalCostUSD)
	}
	if a.Signals.Outcome != "completed" || a.Signals.Grade == nil || *a.Signals.Grade != "A" {
		t.Fatalf("bundle signals = %+v", a.Signals)
	}
	if len(a.Subagents) != 1 || a.Subagents[0].ID != child.SessionID {
		t.Fatalf("bundle subagents = %+v", a.Subagents)
	}
	if a.Tree.SubagentCount != 1 || a.Tree.CostUSD != 2.50 {
		t.Fatalf("bundle rollup = %+v, want 1 subagent at $2.50", a.Tree)
	}
	if len(a.Models) != 1 || a.Models[0] != "claude-fable-5" {
		t.Fatalf("bundle models = %v", a.Models)
	}

	if _, err := st.SessionAuditByID(ctx, 99999999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing session = %v, want ErrNotFound", err)
	}
}

// TestSessionModels pins the audit header's model line: distinct models heaviest first
// by total token volume, empty-model rows dropped, and the whole read capped.
func TestSessionModels(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uid := seedUser(t, st, "grace")
	pid, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatal(err)
	}
	sid := seedSessionWithStats(t, st, uid, pid, "claude", "src-models", 1.0, 10, 10)
	seedUsage(t, st, sid, "claude-opus-4-8", 0.5, 100, 10, 0, "u1")
	seedUsage(t, st, sid, "claude-fable-5", 1.0, 90000, 900, 0, "u2")
	seedUsage(t, st, sid, "claude-fable-5", 1.0, 90000, 900, 0, "u3")

	models, err := st.SessionModels(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || models[0] != "claude-fable-5" || models[1] != "claude-opus-4-8" {
		t.Fatalf("models = %v, want [claude-fable-5 claude-opus-4-8]", models)
	}
}
