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
	"github.com/jssblck/akari/internal/server/store"
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

// TestPublicSessionBodyGateDuringReparse confirms the public transcript fragment
// endpoint (GET /s/{public_id}/body, the "Show earlier" button's hx-get target with
// hx-swap="outerHTML") renders the small reparse banner rather than the full
// PublicReparsePage document for an htmx request while a fleet rebuild is in
// progress: swapping a whole page into the button's own DOM slot would wreck the
// page. A plain navigation to the same URL (no HX-Request header) still gets the
// full reparse stand-in page, matching the authenticated sibling's gateParsed.
func TestPublicSessionBodyGateDuringReparse(t *testing.T) {
	t.Parallel()
	srv, st, worker := newTestServerWithReparse(t)
	seedPublishedPaginationSession(t, st, store.ProjectionDelta{Messages: publicMessages("gated", 240)})

	worker.SetStatusForTest(parse.Status{InProgress: true, Done: 2, Total: 5, Failed: 1})
	t.Cleanup(func() { worker.SetStatusForTest(parse.Status{}) })

	fragURL := srv.URL + "/s/" + publicPaginationID + "/body?before=100&revision=1"

	req, err := http.NewRequest(http.MethodGet, fragURL, nil)
	if err != nil {
		t.Fatalf("build fragment request: %v", err)
	}
	req.Header.Set("HX-Request", "true")
	fragResp := mustDo(t, http.DefaultClient, req)
	frag := readBody(t, fragResp)
	if fragResp.StatusCode != http.StatusOK {
		t.Fatalf("hx-request gated body status = %d, want 200", fragResp.StatusCode)
	}
	if !strings.Contains(frag, "Reparse in progress") {
		t.Fatalf("hx-request gated body should carry the reparse banner, got:\n%s", frag)
	}
	if lower := strings.ToLower(frag); strings.Contains(lower, "<!doctype") || strings.Contains(lower, "<html") {
		t.Fatalf("hx-request gated body should be a fragment, not a full document, got:\n%s", frag)
	}

	navResp := mustGet(t, http.DefaultClient, fragURL)
	nav := readBody(t, navResp)
	if navResp.StatusCode != http.StatusOK {
		t.Fatalf("plain-navigation gated body status = %d, want 200", navResp.StatusCode)
	}
	if lower := strings.ToLower(nav); !strings.Contains(lower, "<!doctype") || !strings.Contains(lower, "<html") {
		t.Fatalf("plain-navigation gated body should be the full reparse page, got:\n%s", nav)
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
