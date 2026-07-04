package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/parse"
)

// registerAdmin registers the first account (which becomes admin) on a fresh
// server and returns a browser-like client holding its session cookie.
func registerAdmin(t *testing.T, srvURL string) *http.Client {
	t.Helper()
	c := newClient(t)
	if _, err := c.PostForm(srvURL+"/register", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("register admin: %v", err)
	}
	return c
}

// TestReparseButtonRequiresAdmin confirms POST /account/reparse is admin-only: the
// admin is redirected back to the account page, a non-admin is forbidden.
func TestReparseButtonRequiresAdmin(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()
	admin := registerAdmin(t, srv.URL)

	// The admin's force-reparse posts and redirects back to /account.
	resp, err := admin.PostForm(srv.URL+"/account/reparse", url.Values{})
	if err != nil {
		t.Fatalf("admin reparse: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Account") {
		t.Fatalf("admin reparse should land back on the account page, got %d:\n%s", resp.StatusCode, body)
	}

	// Seed a non-admin (registered through an invite) and log in as them.
	graceID := func() int64 {
		u, err := st.UserByUsername(ctx, "grace")
		if err != nil {
			t.Fatalf("lookup admin: %v", err)
		}
		return u.ID
	}()
	const invite = "ada-invite-secret"
	if _, err := st.CreateInvite(ctx, auth.HashToken(invite), graceID, "for ada", nil); err != nil {
		t.Fatalf("create invite: %v", err)
	}
	member := newClient(t)
	if _, err := member.PostForm(srv.URL+"/register", url.Values{
		"username": {"ada"}, "password": {"lovelace-1843"}, "invite_token": {invite},
	}); err != nil {
		t.Fatalf("register member: %v", err)
	}

	// A non-admin is forbidden from forcing a reparse.
	resp, err = member.PostForm(srv.URL+"/account/reparse", url.Values{})
	if err != nil {
		t.Fatalf("member reparse: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin reparse status = %d, want 403", resp.StatusCode)
	}
}

// TestParsedEndpointsGateDuringReparse confirms that while a reparse is in
// progress the parsed pages serve the progress stand-in instead of stale rows,
// the status endpoint reports the live counts, the account page stays available,
// and the normal pages return once the reparse clears.
func TestParsedEndpointsGateDuringReparse(t *testing.T) {
	t.Parallel()
	srv, _, worker := newTestServerWithReparse(t)
	c := registerAdmin(t, srv.URL)

	// Before any reparse, the overview renders normally.
	if body := getBody(t, c, srv.URL+"/overview"); !strings.Contains(body, "Overview") {
		t.Fatalf("overview should render normally before a reparse, got:\n%s", body)
	}

	// Force an in-progress reparse without running one.
	worker.SetStatusForTest(parse.Status{InProgress: true, Done: 2, Total: 5, Failed: 1})

	// Parsed pages are gated: they show the progress stand-in. The public homepage
	// at "/" is not parsed data, so it is not in this set.
	for _, path := range []string{"/overview", "/sessions", "/projects"} {
		body := getBody(t, c, srv.URL+path)
		if !strings.Contains(body, "Reparse in progress") {
			t.Fatalf("%s should be gated during a reparse, got:\n%s", path, body)
		}
	}

	// The homepage renders no parsed data, so it stays available during a reparse
	// rather than showing the progress stand-in: the root is deliberately off the
	// gated set.
	if body := getBody(t, c, srv.URL+"/"); !strings.Contains(body, "self-hosted instrument") || strings.Contains(body, "Reparse in progress") {
		t.Fatalf("homepage should render normally during a reparse, got:\n%s", body)
	}

	// The account page is not parsed data, so it stays available (and shows the
	// reparse section).
	if body := getBody(t, c, srv.URL+"/account"); !strings.Contains(body, "Account") {
		t.Fatalf("account page should stay available during a reparse, got:\n%s", body)
	}

	// The status endpoint reports the live counts as JSON.
	resp, err := c.Get(srv.URL + "/api/v1/reparse/status")
	if err != nil {
		t.Fatalf("status endpoint: %v", err)
	}
	var got parse.Status
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	resp.Body.Close()
	if !got.InProgress || got.Done != 2 || got.Total != 5 || got.Failed != 1 {
		t.Fatalf("status = %+v, want in_progress with 2/5 and 1 failed", got)
	}

	// Once the reparse clears, the parsed pages return.
	worker.SetStatusForTest(parse.Status{})
	if body := getBody(t, c, srv.URL+"/overview"); !strings.Contains(body, "Overview") || strings.Contains(body, "Reparse in progress") {
		t.Fatalf("overview should render normally after the reparse clears, got:\n%s", body)
	}
}

// getBody GETs a URL with the client and returns the response body.
func getBody(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	return readBody(t, resp)
}
