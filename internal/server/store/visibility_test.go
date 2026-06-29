package store

import (
	"context"
	"errors"
	"testing"
)

func TestPublishUnpublish(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	ctx := context.Background()

	owner, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.Register(ctx, "ada", "hash", mintInvite(t, st, owner.ID))
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, AnnounceParams{
		UserID: owner.ID, Agent: "claude", SourceSessionID: "sess-1",
		ProjectID: projectID, GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	sid := ann.SessionID

	// A non-owner cannot publish and the session stays internal.
	if _, err := st.PublishSession(ctx, sid, other.ID, "cand-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner publish err = %v, want ErrNotFound", err)
	}
	if d, _ := st.SessionDetailByID(ctx, sid); d.Visibility != "internal" {
		t.Fatalf("session visibility = %q after non-owner publish, want internal", d.Visibility)
	}

	// The owner publishes; the candidate id is adopted.
	pubID, err := st.PublishSession(ctx, sid, owner.ID, "cand-1")
	if err != nil {
		t.Fatalf("owner publish: %v", err)
	}
	if pubID != "cand-1" {
		t.Fatalf("public id = %q, want cand-1", pubID)
	}

	// Re-publishing keeps the original id (the shared link stays valid).
	pubID2, err := st.PublishSession(ctx, sid, owner.ID, "cand-2")
	if err != nil {
		t.Fatalf("re-publish: %v", err)
	}
	if pubID2 != "cand-1" {
		t.Fatalf("re-publish id = %q, want stable cand-1", pubID2)
	}

	// The public id resolves only while public.
	if d, err := st.SessionDetailByPublicID(ctx, "cand-1"); err != nil || d.ID != sid {
		t.Fatalf("lookup by public id: d.ID=%d err=%v", d.ID, err)
	}

	// A non-owner cannot unpublish.
	if err := st.UnpublishSession(ctx, sid, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner unpublish err = %v, want ErrNotFound", err)
	}

	// The owner unpublishes; the link stops resolving.
	if err := st.UnpublishSession(ctx, sid, owner.ID); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if _, err := st.SessionDetailByPublicID(ctx, "cand-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("public lookup after unpublish err = %v, want ErrNotFound", err)
	}
	if d, _ := st.SessionDetailByID(ctx, sid); d.Visibility != "internal" || d.PublicID != nil {
		t.Fatalf("after unpublish visibility=%q publicID=%v, want internal/nil", d.Visibility, d.PublicID)
	}
}

func TestDeleteSessionCascadesAndOrphansBlob(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte("deleted tool body")
	sid := seedSession(t, st, u.ID, projectID, "sess-del")
	proj := ProjectionDelta{
		Messages:  []MessageDelta{{Ordinal: 0, Role: "assistant", Content: "x", HasToolUse: true}},
		ToolCalls: []ProjToolCall{{MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", CallUID: "c1"}},
		ToolResults: []ToolResultDelta{{
			CallUID: "c1", Body: string(body), Bytes: int64(len(body)), MediaType: "text/plain", Status: "ok",
		}},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, proj); err != nil {
		t.Fatal(err)
	}

	if err := st.DeleteSession(ctx, sid); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// The session and everything keyed to it are gone.
	if _, err := st.SessionDetailByID(ctx, sid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session lookup after delete = %v, want ErrNotFound", err)
	}
	for _, tbl := range []string{"messages", "tool_calls", "usage_events", "session_raw"} {
		var n int
		if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM "+tbl+" WHERE session_id = $1", sid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s still has %d rows for deleted session", tbl, n)
		}
	}

	// The blob it referenced is now an orphan a sweep reclaims.
	if removed, err := st.SweepBlobs(ctx); err != nil || removed != 1 {
		t.Fatalf("sweep after delete removed=%d err=%v, want 1", removed, err)
	}

	// Deleting a missing session is ErrNotFound.
	if err := st.DeleteSession(ctx, sid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing = %v, want ErrNotFound", err)
	}
}

// mintInvite creates a redeemable invite and returns the secret's hash, so a
// second user can register in tests.
func mintInvite(t *testing.T, st *Store, adminID int64) string {
	t.Helper()
	hash := hashHex("invite-" + itoa(int(adminID)))
	if _, err := st.CreateInvite(context.Background(), hash, adminID, "test", nil); err != nil {
		t.Fatalf("create invite: %v", err)
	}
	return hash
}
