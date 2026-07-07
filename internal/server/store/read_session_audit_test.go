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

// TestSessionAuditByID pins the one-snapshot audit bundle: the detail, signals, and
// subagents the instruments read side by side all arrive from a single read and agree
// with the individual reads on quiet data. (The snapshot property itself is the
// repeatable-read transaction; this pins the wiring.)
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

	if _, err := st.SessionAuditByID(ctx, 99999999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing session = %v, want ErrNotFound", err)
	}
}

// TestSessionSnapshotByID pins the one-transaction session view: the audit bundle, the
// tail window, and the whole-session shape (outline rows plus tool metadata) all arrive
// together, so the page can never render one projection's window beside another's
// outline. TestSessionAppendByID beside it pins the append variant's shape gating.
func TestSessionSnapshotByID(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedTurns(t, st, "grace", 30)
	// Two rows sharing a call_uid: a replayed turn, so the snapshot's duplicate-id
	// count (read with the same tool rows the page renders) reports one repeated id.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO tool_calls (session_id, message_ordinal, call_index, tool_name, category, call_uid)
		 VALUES ($1, 3, 0, 'Edit', 'edit', 'toolu_dup'), ($1, 5, 0, 'Edit', 'edit', 'toolu_dup')`, sid); err != nil {
		t.Fatal(err)
	}

	snap, err := st.SessionSnapshotByID(ctx, sid)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Audit.Detail.ID != sid {
		t.Fatalf("snapshot detail = %d, want %d", snap.Audit.Detail.ID, sid)
	}
	if len(snap.Page.Msgs) != 30 {
		t.Fatalf("snapshot window = %d rows, want all 30", len(snap.Page.Msgs))
	}
	if len(snap.Outline) != 30 {
		t.Fatalf("snapshot outline = %d rows, want 30", len(snap.Outline))
	}
	if len(snap.Tools) != 2 || snap.Tools[0].MessageOrdinal != 3 {
		t.Fatalf("snapshot tools = %+v, want the ordinal-3 and ordinal-5 calls", snap.Tools)
	}
	if snap.DupIDs != 1 {
		t.Fatalf("snapshot DupIDs = %d, want 1 repeated call id", snap.DupIDs)
	}

	if _, err := st.SessionSnapshotByID(ctx, 99999999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing session = %v, want ErrNotFound", err)
	}
}

// TestSessionAppendByID pins the append snapshot: rows past the cursor with the shape
// riding along, a quiet tick with the shape skipped (nil, so the fragment ships no
// swap), and an empty seed for a cursor over an emptied projection (the handler's
// resync signal).
func TestSessionAppendByID(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	const total = 30
	sid := seedTurns(t, st, "grace", total)

	snap, err := st.SessionAppendByID(ctx, sid, 27)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if len(snap.Page.Msgs) != 2 || snap.Page.Msgs[0].Ordinal != 28 {
		t.Fatalf("append rows = %v, want [28 29]", ordinals(snap.Page.Msgs))
	}
	if len(snap.Outline) != total {
		t.Fatalf("append shape outline = %d rows, want %d", len(snap.Outline), total)
	}

	quiet, err := st.SessionAppendByID(ctx, sid, total-1)
	if err != nil {
		t.Fatalf("quiet append: %v", err)
	}
	if len(quiet.Page.Msgs) != 0 || quiet.Outline != nil || quiet.Tools != nil {
		t.Fatalf("quiet tick should carry no rows and no shape, got %d rows, outline %d",
			len(quiet.Page.Msgs), len(quiet.Outline))
	}

	// A cursor over an emptied projection (a rebuild removed every message) yields an
	// empty seed, the handler's signal to resync rather than leave stale DOM rows.
	if _, err := st.Pool.Exec(ctx, `DELETE FROM messages WHERE session_id = $1`, sid); err != nil {
		t.Fatal(err)
	}
	gone, err := st.SessionAppendByID(ctx, sid, 5)
	if err != nil {
		t.Fatalf("append over emptied projection: %v", err)
	}
	if len(gone.Page.Msgs) != 0 || len(gone.Page.Seed) != 0 {
		t.Fatalf("emptied projection should return no rows and no seed, got msgs=%v seed=%v",
			ordinals(gone.Page.Msgs), ordinals(gone.Page.Seed))
	}
}
