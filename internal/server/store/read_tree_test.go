package store_test

import (
	"context"
	"math"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

// setParent links a child session to its parent under the given relationship, the shape
// the ingest path writes when it recognizes a subagent fan-out or a resumed session. The
// tree rollup walks these edges, so a test builds the tree by stamping them directly.
func setParent(t *testing.T, st *store.Store, ctx context.Context, childID, parentID int64, rel string) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx,
		`UPDATE sessions SET parent_session_id = $2, relationship_type = $3 WHERE id = $1`,
		childID, parentID, rel); err != nil {
		t.Fatalf("set parent for session %d: %v", childID, err)
	}
}

// setCostIncomplete marks a session's cost as a floor (an unpriced model, a missing rate),
// so the rollup can be checked for propagating the flag up the subtree.
func setCostIncomplete(t *testing.T, st *store.Store, ctx context.Context, sid int64) {
	t.Helper()
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET cost_incomplete = true WHERE id = $1`, sid); err != nil {
		t.Fatalf("set cost_incomplete for session %d: %v", sid, err)
	}
}

// TestTreeRollup pins the feed's whole-work-item rollup against a hand-built fan-out: a
// root folds in its subagents at every depth (not just its direct children), a continuation
// is its own work item and never folds into the session it continued, and the incomplete
// flag propagates when any session in the subtree is unpriced. The rollup rides only the
// non-subagent feed rows, since a subagent is machinery under its parent, not a row of its
// own.
func TestTreeRollup(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)

	// Root A fans out two direct subagents and one grandchild subagent (depth 2), so the
	// rollup must recurse rather than count only direct children.
	rootA := seedSession(t, st, uid, pid, "root-a")
	setSessionCost(t, st, ctx, rootA, 1.00)
	subA1 := seedSession(t, st, uid, pid, "sub-a1")
	setSessionCost(t, st, ctx, subA1, 2.00)
	setParent(t, st, ctx, subA1, rootA, "subagent")
	subA2 := seedSession(t, st, uid, pid, "sub-a2")
	setSessionCost(t, st, ctx, subA2, 3.00)
	setParent(t, st, ctx, subA2, rootA, "subagent")
	setCostIncomplete(t, st, ctx, subA2) // an unpriced subagent makes the whole subtree a floor
	subA1a := seedSession(t, st, uid, pid, "sub-a1a")
	setSessionCost(t, st, ctx, subA1a, 0.50)
	setParent(t, st, ctx, subA1a, subA1, "subagent")

	// Root B spawned nothing: its rollup is just its own cost with a zero count.
	rootB := seedSession(t, st, uid, pid, "root-b")
	setSessionCost(t, st, ctx, rootB, 0.40)

	// Continuation C resumes root A and fans out its own subagent. It is a separate feed
	// row, so folding it into A would double-count it; the rollup must not follow the
	// continuation edge from A, but must still fold C's own subagent into C.
	contC := seedSession(t, st, uid, pid, "cont-c")
	setSessionCost(t, st, ctx, contC, 5.00)
	setParent(t, st, ctx, contC, rootA, "continuation")
	subC1 := seedSession(t, st, uid, pid, "sub-c1")
	setSessionCost(t, st, ctx, subC1, 0.10)
	setParent(t, st, ctx, subC1, contC, "subagent")

	// IncludeEmpty because the seeded sessions carry no messages; the feed still hides
	// subagents, so only the three non-subagent rows (A, B, C) come back with rollups.
	rows, _, err := st.ListAllSessions(ctx, store.SessionFilter{ProjectID: pid, IncludeEmpty: true})
	if err != nil {
		t.Fatalf("list all sessions: %v", err)
	}
	byID := make(map[int64]store.SessionRow, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	if len(rows) != 3 {
		t.Fatalf("feed returned %d rows, want 3 (roots A, B and continuation C; subagents hidden)", len(rows))
	}
	for _, sub := range []int64{subA1, subA2, subA1a, subC1} {
		if _, ok := byID[sub]; ok {
			t.Errorf("subagent %d surfaced as a feed row; subagents must stay hidden", sub)
		}
	}

	cases := []struct {
		name       string
		id         int64
		count      int
		cost       float64
		incomplete bool
	}{
		// A: itself ($1) + two subagents ($2, $3) + one grandchild ($0.50) = $6.50 over 3
		// subagents; incomplete because subA2 is unpriced. C's $5.10 is NOT folded in.
		{"root A", rootA, 3, 6.50, true},
		// B fanned out nothing: its own $0.40, zero subagents, fully priced.
		{"root B", rootB, 0, 0.40, false},
		// C: itself ($5) + its one subagent ($0.10) = $5.10 over 1 subagent, fully priced.
		{"continuation C", contC, 1, 5.10, false},
	}
	for _, c := range cases {
		tr := byID[c.id].Tree
		if tr.SubagentCount != c.count {
			t.Errorf("%s: SubagentCount = %d, want %d", c.name, tr.SubagentCount, c.count)
		}
		if math.Abs(tr.CostUSD-c.cost) > 1e-9 {
			t.Errorf("%s: CostUSD = %v, want %v", c.name, tr.CostUSD, c.cost)
		}
		if tr.CostIncomplete != c.incomplete {
			t.Errorf("%s: CostIncomplete = %v, want %v", c.name, tr.CostIncomplete, c.incomplete)
		}
	}
}
