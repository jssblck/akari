package store

import (
	"context"
	"errors"
	"testing"
)

func TestPublishUnpublish(t *testing.T) {
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
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari")
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
