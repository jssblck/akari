package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"

	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/store"
)

// seedToolSession announces a session for the user and writes a projection with a
// single tool call whose result body is the given content, returning the session
// id and the body's sha256.
func seedToolSession(t *testing.T, st *store.Store, userID, projectID int64, source string, body []byte) (int64, string) {
	t.Helper()
	ctx := context.Background()
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: userID, Agent: "claude", SourceSessionID: source,
		ProjectID: projectID, GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce %s: %v", source, err)
	}
	delta := store.ProjectionDelta{
		MessagesAdded: 1,
		Messages:      []store.MessageDelta{{Ordinal: 0, Role: "assistant", Content: "x", HasToolUse: true}},
		ToolCalls: []store.ProjToolCall{{
			MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", Category: "read", CallUID: "call-" + source,
		}},
		ToolResults: []store.ToolResultDelta{{
			CallUID: "call-" + source, Body: string(body), Bytes: int64(len(body)),
			MediaType: "text/plain", Status: "ok",
		}},
	}
	if err := st.ApplyProjectionDelta(ctx, ann.SessionID, delta); err != nil {
		t.Fatalf("apply projection %s: %v", source, err)
	}
	return ann.SessionID, store.HashBytes(body)
}

func TestDeleteSessionAuthz(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()

	admin, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register admin: %v", err)
	}
	if _, err := st.CreateInvite(ctx, auth.HashToken("inv1"), admin.ID, "", nil); err != nil {
		t.Fatalf("invite: %v", err)
	}
	user, err := st.Register(ctx, "ada", mustHash(t, "lovelace-1843"), auth.HashToken("inv1"))
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	login := func(username, password string) *http.Client {
		c := newClient(t)
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := c.PostForm(srv.URL+"/login", url.Values{"username": {username}, "password": {password}})
		if err != nil {
			t.Fatalf("login %s: %v", username, err)
		}
		resp.Body.Close()
		return c
	}
	del := func(c *http.Client, id int64) int {
		resp, err := c.PostForm(srv.URL+fmt.Sprintf("/sessions/%d/delete", id), url.Values{})
		if err != nil {
			t.Fatalf("delete %d: %v", id, err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	seed := func(owner int64, src string) int64 {
		ann, err := st.Announce(ctx, store.AnnounceParams{
			UserID: owner, Agent: "claude", SourceSessionID: src,
			ProjectID: projectID, GitBranch: "main", Cwd: "/x", Machine: "m",
		})
		if err != nil {
			t.Fatalf("announce %s: %v", src, err)
		}
		return ann.SessionID
	}

	adaClient := login("ada", "lovelace-1843")
	graceClient := login("grace", "hopper-1906")

	// A non-owner, non-admin cannot delete someone else's session.
	graceSession := seed(admin.ID, "grace-1")
	if code := del(adaClient, graceSession); code != http.StatusForbidden {
		t.Fatalf("non-owner delete status = %d, want 403", code)
	}
	if _, err := st.SessionDetailByID(ctx, graceSession); err != nil {
		t.Fatalf("session should survive a forbidden delete: %v", err)
	}

	// The owner can delete their own session.
	adaSession := seed(user.ID, "ada-1")
	if code := del(adaClient, adaSession); code != http.StatusSeeOther {
		t.Fatalf("owner delete status = %d, want 303", code)
	}
	if _, err := st.SessionDetailByID(ctx, adaSession); err == nil {
		t.Fatal("owner-deleted session still present")
	}

	// An admin can delete another user's session.
	adaSession2 := seed(user.ID, "ada-2")
	if code := del(graceClient, adaSession2); code != http.StatusSeeOther {
		t.Fatalf("admin delete status = %d, want 303", code)
	}
	if _, err := st.SessionDetailByID(ctx, adaSession2); err == nil {
		t.Fatal("admin-deleted session still present")
	}
}

func TestBlobServingAccessControl(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	c := newClient(t)

	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	// Session A holds a "public" body and gets published. Session B holds a
	// different, secret body and stays internal.
	pubBody := []byte("public tool output")
	sessA, shaA := seedToolSession(t, st, owner.ID, projectID, "sess-a", pubBody)
	secretBody := []byte("INTERNAL SECRET OUTPUT")
	_, shaB := seedToolSession(t, st, owner.ID, projectID, "sess-b", secretBody)

	candidate := "pubcap-aaaa"
	if _, err := st.PublishSession(ctx, sessA, owner.ID, candidate); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}

	// Authenticated owner can fetch A's blob through A.
	get := func(client *http.Client, path string) (*http.Response, string) {
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		return resp, readBody(t, resp)
	}

	resp, body := get(c, fmt.Sprintf("/api/v1/session/%d/blob/%s", sessA, shaA))
	if resp.StatusCode != http.StatusOK || body != string(pubBody) {
		t.Fatalf("authed blob A: status=%d body=%q", resp.StatusCode, body)
	}

	// Authed: fetching B's hash *through session A* must 404. A does not reference
	// it, so the content-addressed dedup cannot leak it across sessions.
	resp, _ = get(c, fmt.Sprintf("/api/v1/session/%d/blob/%s", sessA, shaB))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-session blob (authed) status=%d, want 404", resp.StatusCode)
	}

	// Anonymous: the published session serves its own blob.
	anon := newClient(t)
	resp, body = get(anon, fmt.Sprintf("/s/%s/blob/%s", candidate, shaA))
	if resp.StatusCode != http.StatusOK || body != string(pubBody) {
		t.Fatalf("public blob A: status=%d body=%q", resp.StatusCode, body)
	}

	// Anonymous: the secret body must not be reachable through the public session,
	// even by hash.
	resp, _ = get(anon, fmt.Sprintf("/s/%s/blob/%s", candidate, shaB))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("public secret blob status=%d, want 404", resp.StatusCode)
	}

	// Anonymous: the authenticated blob route is closed to them entirely.
	resp, _ = get(anon, fmt.Sprintf("/api/v1/session/%d/blob/%s", sessA, shaA))
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("anon hit authed blob route, status=%d", resp.StatusCode)
	}

	// A malformed hash is a 404, not a 500.
	resp, _ = get(c, fmt.Sprintf("/api/v1/session/%d/blob/not-a-hash", sessA))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("malformed hash status=%d, want 404", resp.StatusCode)
	}
}
