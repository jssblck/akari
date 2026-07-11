package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/store"
)

func TestAuthenticatedResponsesArePrivateNoStore(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	client := registerAdmin(t, srv.URL)

	for _, path := range []string{"/", "/guide", "/account", "/api/v1/tokens"} {
		resp := mustGet(t, client, srv.URL+path)
		resp.Body.Close()
		if got := resp.Header.Get("Cache-Control"); got != "private, no-store" {
			t.Errorf("GET %s Cache-Control = %q, want private, no-store", path, got)
		}
	}
}

func TestExplicitCachePoliciesOverrideAuthenticatedDefault(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	client := registerAdmin(t, srv.URL)

	resp := mustGet(t, client, srv.URL+"/overview")
	resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "private, max-age=30" {
		t.Fatalf("overview Cache-Control = %q, want private dashboard cache", got)
	}

	resp = mustGet(t, http.DefaultClient, srv.URL+"/og.png")
	resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); !strings.HasPrefix(got, "public, max-age=") {
		t.Fatalf("landing card Cache-Control = %q, want explicit public cache", got)
	}
}

func TestRevocablePublicPagesAreNoStoreInEveryState(t *testing.T) {
	t.Parallel()
	srv, st, worker := newTestServerWithReparse(t)
	ctx := context.Background()

	owner, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.PublishOverview(ctx, owner.ID); err != nil {
		t.Fatalf("publish overview: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if err := st.PublishProjectOverview(ctx, projectID); err != nil {
		t.Fatalf("publish project: %v", err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: owner.ID, Agent: "claude", SourceSessionID: "no-store-public",
		ProjectID: projectID, Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	rebuildWith(t, st, ann.SessionID, store.ProjectionDelta{Messages: []store.MessageDelta{{
		Ordinal: 0, Role: "user", Content: "Audit the public cache policy",
	}}})
	const publicID = "cache-policy-public-id"
	if _, err := st.PublishSession(ctx, ann.SessionID, owner.ID, publicID); err != nil {
		t.Fatalf("publish session: %v", err)
	}

	paths := []string{"/u/grace", fmt.Sprintf("/p/%d", projectID), "/s/" + publicID}
	assertNoStore := func(state string, wantStatus int) {
		t.Helper()
		for _, path := range paths {
			resp := mustGet(t, http.DefaultClient, srv.URL+path)
			resp.Body.Close()
			if resp.StatusCode != wantStatus {
				t.Errorf("%s GET %s = %d, want %d", state, path, resp.StatusCode, wantStatus)
			}
			if got := resp.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("%s GET %s Cache-Control = %q, want no-store", state, path, got)
			}
		}
	}

	assertNoStore("published", http.StatusOK)
	worker.SetStatusForTest(parse.Status{InProgress: true, Total: 1})
	assertNoStore("reparse", http.StatusOK)
	worker.SetStatusForTest(parse.Status{})

	if err := st.UnpublishOverview(ctx, owner.ID); err != nil {
		t.Fatalf("unpublish overview: %v", err)
	}
	if err := st.UnpublishProjectOverview(ctx, projectID); err != nil {
		t.Fatalf("unpublish project: %v", err)
	}
	if err := st.UnpublishSession(ctx, ann.SessionID, owner.ID); err != nil {
		t.Fatalf("unpublish session: %v", err)
	}
	assertNoStore("revoked", http.StatusNotFound)
}
