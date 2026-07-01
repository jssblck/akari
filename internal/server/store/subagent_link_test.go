package store_test

import (
	"context"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestAnnounceLinksSubagent pins the parent/child wiring: a subagent session, whose source
// id nests under its parent's ("<parent>/subagents/..."), gets parent_session_id and
// relationship_type set on announce, and Subagents(parent) returns it. A top-level session
// is left unlinked, and either ingest order resolves because linking re-runs on every
// announce.
func TestAnnounceLinksSubagent(t *testing.T) {
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
	parentOf := func(sid int64) (*int64, string) {
		t.Helper()
		var pp *int64
		var rel string
		if err := st.Pool.QueryRow(ctx, "SELECT parent_session_id, relationship_type FROM sessions WHERE id=$1", sid).Scan(&pp, &rel); err != nil {
			t.Fatalf("read session %d: %v", sid, err)
		}
		return pp, rel
	}

	// Parent-first: the parent already exists, so the child links on its own announce.
	parent := announce("parent-uuid")
	child := announce("parent-uuid/subagents/agent-abc")
	if pp, rel := parentOf(child); pp == nil || *pp != parent || rel != "subagent" {
		t.Fatalf("child link = (%v, %q), want (%d, subagent)", pp, rel, parent)
	}
	if pp, rel := parentOf(parent); pp != nil || rel != "" {
		t.Fatalf("parent should be top-level, got (%v, %q)", pp, rel)
	}
	subs, err := st.Subagents(ctx, parent)
	if err != nil {
		t.Fatalf("subagents: %v", err)
	}
	if len(subs) != 1 || subs[0].ID != child {
		t.Fatalf("Subagents(parent) = %+v, want the one child %d", subs, child)
	}

	// Child-first: a child announced before its parent exists cannot link yet, but the
	// parent adopts it the moment it lands, so ingest order does not matter and no
	// re-announce is needed.
	orphan := announce("late-parent/subagents/agent-xyz")
	if pp, _ := parentOf(orphan); pp != nil {
		t.Fatalf("child announced before its parent should be unlinked, got parent %d", *pp)
	}
	lateParent := announce("late-parent")
	if pp, rel := parentOf(orphan); pp == nil || *pp != lateParent || rel != "subagent" {
		t.Fatalf("orphan adopted on parent announce = (%v, %q), want (%d, subagent)", pp, rel, lateParent)
	}
}

// TestBackfillLinksExistingSubagents pins the one-time backfill in migration 0019: subagent
// sessions stored before ingest-time linking shipped are adopted by matching their
// source-id prefix to a parent of the same user. The statement here mirrors the migration,
// so a change to the match (the "/subagents/" split, the position guard, or the same-user
// scoping) fails this test.
func TestBackfillLinksExistingSubagents(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uid := seedUser(t, st, "grace")
	otherID := seedUser(t, st, "ada")
	pid, err := st.UpsertProject(ctx, "github.com/ada/engine", "github.com", "ada", "engine", "engine", "remote")
	if err != nil {
		t.Fatal(err)
	}

	// Insert orphans directly, bypassing announce so nothing is pre-linked.
	insert := func(userID int64, src string) int64 {
		t.Helper()
		var id int64
		if err := st.Pool.QueryRow(ctx,
			`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine)
			 VALUES ($1,$2,'claude',$3,'box') RETURNING id`,
			userID, pid, src).Scan(&id); err != nil {
			t.Fatalf("insert %q: %v", src, err)
		}
		return id
	}
	parent := insert(uid, "p1")
	child := insert(uid, "p1/subagents/agent-1")
	crossUser := insert(otherID, "p1/subagents/agent-2") // parent "p1" is a different user's
	standalone := insert(uid, "solo")

	if _, err := st.Pool.Exec(ctx,
		`UPDATE sessions AS child
		    SET parent_session_id = parent.id, relationship_type = 'subagent'
		   FROM sessions AS parent
		  WHERE child.agent = 'claude'
		    AND child.parent_session_id IS NULL
		    AND position('/subagents/' IN child.source_session_id) > 1
		    AND parent.user_id = child.user_id
		    AND parent.agent = 'claude'
		    AND parent.source_session_id = split_part(child.source_session_id, '/subagents/', 1)`); err != nil {
		t.Fatalf("backfill: %v", err)
	}

	parentID := func(sid int64) *int64 {
		t.Helper()
		var pp *int64
		if err := st.Pool.QueryRow(ctx, "SELECT parent_session_id FROM sessions WHERE id=$1", sid).Scan(&pp); err != nil {
			t.Fatalf("read %d: %v", sid, err)
		}
		return pp
	}
	if pp := parentID(child); pp == nil || *pp != parent {
		t.Fatalf("child parent = %v, want %d", pp, parent)
	}
	if pp := parentID(crossUser); pp != nil {
		t.Fatalf("cross-user child must not link, got parent %d", *pp)
	}
	if pp := parentID(standalone); pp != nil {
		t.Fatalf("standalone must stay top-level, got parent %d", *pp)
	}
}
