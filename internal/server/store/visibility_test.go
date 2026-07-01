package store_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

func TestPublishUnpublish(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
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
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: owner.ID, Agent: "claude", SourceSessionID: "sess-1",
		ProjectID: projectID, GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	sid := ann.SessionID

	// A non-owner cannot publish and the session stays internal.
	if _, err := st.PublishSession(ctx, sid, other.ID, "cand-x"); !errors.Is(err, store.ErrNotFound) {
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
	if err := st.UnpublishSession(ctx, sid, other.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("non-owner unpublish err = %v, want ErrNotFound", err)
	}

	// The owner unpublishes; the link stops resolving.
	if err := st.UnpublishSession(ctx, sid, owner.ID); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if _, err := st.SessionDetailByPublicID(ctx, "cand-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("public lookup after unpublish err = %v, want ErrNotFound", err)
	}
	if d, _ := st.SessionDetailByID(ctx, sid); d.Visibility != "internal" || d.PublicID != nil {
		t.Fatalf("after unpublish visibility=%q publicID=%v, want internal/nil", d.Visibility, d.PublicID)
	}
}

func TestPublishUnpublishOverview(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	owner, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}

	// A fresh account is not public.
	if u, _ := st.UserByID(ctx, owner.ID); u.OverviewPublic {
		t.Fatalf("fresh account public=%v, want false", u.OverviewPublic)
	}
	// While unpublished, the username lookup finds nothing.
	if _, err := st.PublicOverviewUser(ctx, "grace"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lookup before publish = %v, want ErrNotFound", err)
	}

	// Publishing flips the gate; the page resolves by username.
	if err := st.PublishOverview(ctx, owner.ID); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if u, err := st.PublicOverviewUser(ctx, "grace"); err != nil || u.ID != owner.ID {
		t.Fatalf("lookup by username: u.ID=%d err=%v", u.ID, err)
	}

	// Disabling hides the page (the link stops resolving).
	if err := st.UnpublishOverview(ctx, owner.ID); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	if _, err := st.PublicOverviewUser(ctx, "grace"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("lookup after unpublish = %v, want ErrNotFound", err)
	}
	if u, _ := st.UserByID(ctx, owner.ID); u.OverviewPublic {
		t.Fatalf("after unpublish public=%v, want false", u.OverviewPublic)
	}

	// Re-publishing brings the same /u/<username> back.
	if err := st.PublishOverview(ctx, owner.ID); err != nil {
		t.Fatalf("re-publish: %v", err)
	}
	if u, err := st.PublicOverviewUser(ctx, "grace"); err != nil || u.ID != owner.ID {
		t.Fatalf("lookup after re-publish: u.ID=%d err=%v", u.ID, err)
	}

	// Toggling a user that does not exist touches no row and is ErrNotFound rather
	// than a silent no-op, so a caller cannot mistake "nothing happened" for success.
	missing := owner.ID + 9999
	if err := st.PublishOverview(ctx, missing); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("publish missing user = %v, want ErrNotFound", err)
	}
	if err := st.UnpublishOverview(ctx, missing); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unpublish missing user = %v, want ErrNotFound", err)
	}
}

func TestDeleteSessionCascadesAndOrphansBlob(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
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
	proj := store.ProjectionDelta{
		Messages:  []store.MessageDelta{{Ordinal: 0, Role: "assistant", Content: "x", HasToolUse: true}},
		ToolCalls: []store.ProjToolCall{{MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", CallUID: "c1"}},
		ToolResults: []store.ToolResultDelta{{
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
	if _, err := st.SessionDetailByID(ctx, sid); !errors.Is(err, store.ErrNotFound) {
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
	if err := st.DeleteSession(ctx, sid); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete missing = %v, want ErrNotFound", err)
	}
}

// mintInvite creates a redeemable invite and returns the secret's hash, so a
// second user can register in tests.
func mintInvite(t *testing.T, st *store.Store, adminID int64) string {
	t.Helper()
	hash := hashHex("invite-" + strconv.Itoa(int(adminID)))
	if _, err := st.CreateInvite(context.Background(), hash, adminID, "test", nil); err != nil {
		t.Fatalf("create invite: %v", err)
	}
	return hash
}
