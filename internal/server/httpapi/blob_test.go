package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"testing"

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
	proj := store.Projection{
		ParserVersion: 1, MessageCount: 1,
		Messages: []store.ProjMessage{{Ordinal: 0, Role: "assistant", Content: "x", HasToolUse: true}},
		ToolCalls: []store.ProjToolCall{{
			MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", Category: "read",
			HasResult: true, ResultBody: body, ResultBytes: int64(len(body)),
			ResultMediaType: "text/plain", ResultStatus: "ok",
		}},
	}
	if err := st.WriteProjection(ctx, ann.SessionID, 0, proj); err != nil {
		t.Fatalf("write projection %s: %v", source, err)
	}
	return ann.SessionID, store.HashBytes(body)
}

func TestBlobServingAccessControl(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	c := newClient(t)

	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari")
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
