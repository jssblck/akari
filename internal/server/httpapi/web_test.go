package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"time"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/ogimage"
	"github.com/jssblck/akari/internal/server/reparse"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
	"github.com/jssblck/akari/internal/server/web"
)

// mustHash hashes a password for seeding a test account directly via the store.
func mustHash(t *testing.T, password string) string {
	t.Helper()
	h, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	return h
}

// newTestServer brings up a full Routes() handler backed by its own isolated,
// freshly migrated database, returning the server and its store. The database is
// created and force-dropped by the storetest package, so tests run safely in
// parallel; it is skipped unless AKARI_TEST_DATABASE_URL is set.
func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	srv, st, _ := newTestServerWithReparse(t)
	return srv, st
}

// newTestServerWithReparse is newTestServer that also returns the reparse service
// wired into the server, so a test can force its status to exercise the UI gating.
func newTestServerWithReparse(t *testing.T) (*httptest.Server, *store.Store, *reparse.Service) {
	t.Helper()
	st := storetest.NewStore(t)
	rp := reparse.New(context.Background(), st)
	srv := httptest.NewServer(New(st, config.Server{}, rp).Routes())
	t.Cleanup(srv.Close)
	return srv, st, rp
}

// newClient returns an http.Client that follows redirects and keeps cookies, so
// it behaves like a browser through the login flow.
func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{Jar: jar}
}

func TestWebFlow(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	c := newClient(t)

	// The unauthenticated root serves the landing page (explaining akari), not a
	// redirect to login: the request stays on / and renders the marketing hero
	// with links into sign-in and registration.
	resp, err := c.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unauthenticated / = %d, want 200 landing page", resp.StatusCode)
	}
	if resp.Request.URL.Path != "/" {
		t.Fatalf("unauthenticated / redirected to %q, want the landing page at /", resp.Request.URL.Path)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "self-hosted instrument") || !strings.Contains(body, `href="/guide"`) {
		t.Fatalf("unauthenticated / should render the landing page, got:\n%s", body)
	}

	// An authed-only read page still redirects an anonymous visitor to login.
	resp, err = c.Get(srv.URL + "/projects")
	if err != nil {
		t.Fatalf("get /projects: %v", err)
	}
	body = readBody(t, resp)
	if !strings.Contains(body, "Log in") {
		t.Fatalf("unauthenticated /projects should land on login page, got:\n%s", body)
	}

	// Register the first account (becomes admin, no invite needed).
	resp, err = c.PostForm(srv.URL+"/register", url.Values{
		"username": {"grace"},
		"password": {"hopper-1906"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	body = readBody(t, resp)
	if !strings.Contains(body, "Overview") {
		t.Fatalf("after register should land on the overview, got:\n%s", body)
	}

	// The overview is now the landing surface, reachable directly with the
	// session cookie; it shows the signed-in user in the sidebar.
	resp, err = c.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get / authed: %v", err)
	}
	body = readBody(t, resp)
	if !strings.Contains(body, "grace") || !strings.Contains(body, "Overview") {
		t.Fatalf("overview page missing expected content, got:\n%s", body)
	}
	// The standalone search page was retired, so the sidebar must not link to it.
	if strings.Contains(body, `href="/search"`) {
		t.Fatalf("sidebar still links to the removed search page, got:\n%s", body)
	}

	// The projects table moved to /projects; with no projects yet it shows its
	// empty state.
	resp, err = c.Get(srv.URL + "/projects")
	if err != nil {
		t.Fatalf("get /projects: %v", err)
	}
	body = readBody(t, resp)
	if !strings.Contains(body, "No git-remote projects yet") {
		t.Fatalf("projects page missing empty state, got:\n%s", body)
	}

	// The global sessions list renders with no sessions yet.
	resp, err = c.Get(srv.URL + "/sessions")
	if err != nil {
		t.Fatalf("get /sessions: %v", err)
	}
	body = readBody(t, resp)
	if !strings.Contains(body, "Sessions") {
		t.Fatalf("sessions page missing heading, got:\n%s", body)
	}

	// Create an ingest token via the account form; the secret is flashed once.
	resp, err = c.PostForm(srv.URL+"/account/tokens", url.Values{
		"name":  {"laptop"},
		"scope": {"ingest"},
	})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	body = readBody(t, resp)
	if !strings.Contains(body, "New token") || !strings.Contains(body, "laptop") {
		t.Fatalf("account page should show the new token, got:\n%s", body)
	}

	// A reload no longer shows the secret (flash cleared).
	resp, err = c.Get(srv.URL + "/account")
	if err != nil {
		t.Fatalf("reload account: %v", err)
	}
	body = readBody(t, resp)
	if strings.Contains(body, "New token (shown once)") {
		t.Fatalf("flash should be cleared on reload, got:\n%s", body)
	}

	// Admin sees the invite form.
	if !strings.Contains(body, "Invites") {
		t.Fatalf("admin account page should show invites, got:\n%s", body)
	}

	// The /search route was removed with the page; an authenticated request for
	// it now falls through to a 404 rather than rendering anything.
	resp, err = c.Get(srv.URL + "/search")
	if err != nil {
		t.Fatalf("get /search: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /search after removal = %d, want 404", resp.StatusCode)
	}

	// Logout clears the session and lands on the login page.
	resp, err = c.PostForm(srv.URL+"/logout", url.Values{})
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	body = readBody(t, resp)
	if !strings.Contains(body, "Log in") {
		t.Fatalf("after logout should be login page, got:\n%s", body)
	}

	// With the session gone, the root is the public landing page again rather
	// than the signed-in overview.
	resp, err = c.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get / after logout: %v", err)
	}
	body = readBody(t, resp)
	if !strings.Contains(body, "self-hosted instrument") {
		t.Fatalf("after logout / should render the landing page, got:\n%s", body)
	}
}

// TestRootNonFullCredentialGetsLanding pins handleRoot's gate: only a full-scope
// credential reaches the overview, so a read- or ingest-scope bearer token (a
// non-browser credential pointed at the UI root) is treated as logged out and
// gets the landing page, not the signed-in overview. This exercises the branch
// TestWebFlow leaves uncovered, which only drives an anonymous request and a
// full-scope browser session.
func TestRootNonFullCredentialGetsLanding(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register user: %v", err)
	}
	for _, tc := range []struct{ scope, token string }{
		{scopeRead, "read-secret-token"},
		{scopeIngest, "ingest-secret-token"},
	} {
		if _, err := st.CreateAPIToken(ctx, u.ID, tc.scope, tc.scope, auth.HashToken(tc.token)); err != nil {
			t.Fatalf("create %s token: %v", tc.scope, err)
		}
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/", nil)
		req.Header.Set("Authorization", "Bearer "+tc.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("get / with %s token: %v", tc.scope, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("get / with %s token = %d, want 200 landing page", tc.scope, resp.StatusCode)
		}
		body := readBody(t, resp)
		if !strings.Contains(body, "self-hosted instrument") {
			t.Fatalf("%s-scope root should render the landing page, got:\n%s", tc.scope, body)
		}
		if strings.Contains(body, `class="sidebar"`) {
			t.Fatalf("%s-scope root should not render the signed-in overview shell, got:\n%s", tc.scope, body)
		}
	}
}

// TestStandaloneOrphanedIndex drives the real ingest endpoint with both a remote
// and non-remote kinds and confirms the projects index lists only the git-remote
// project: standalone and orphaned folders are kept off this surface (they reach
// the reader through the Sessions filter rail), while drilling straight into a
// local folder still shows its state and path.
func TestStandaloneOrphanedIndex(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()
	c := newClient(t)

	// Register the first admin (browser cookie) and mint an ingest token whose
	// raw secret we control, so we can call the ingest API directly.
	if _, err := c.PostForm(srv.URL+"/register", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	u, err := st.UserByUsername(ctx, "grace")
	if err != nil {
		t.Fatalf("lookup user: %v", err)
	}
	const token = "ingest-secret-token"
	if _, err := st.CreateAPIToken(ctx, u.ID, "laptop", "ingest", auth.HashToken(token)); err != nil {
		t.Fatalf("create token: %v", err)
	}

	announce := func(kind, source, cwd, remote string) {
		t.Helper()
		payload := map[string]string{
			"agent": "claude", "source_session_id": source, "kind": kind,
			"cwd": cwd, "machine": "grace-laptop",
		}
		if remote != "" {
			payload["project_remote"] = remote
		}
		body, _ := json.Marshal(payload)
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/ingest/session", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("announce %s: %v", source, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("announce %s: status %d", source, resp.StatusCode)
		}
	}

	announce("remote", "sess-remote", "/home/grace/akari", "github.com/grace-hopper/akari")
	announce("standalone", "sess-standalone", "/home/grace/scratch", "")
	announce("orphaned", "sess-orphaned", "/home/grace/deleted", "")

	// The projects index lists the git-remote project and nothing else: no local
	// folders, no "Sessions" section, and never the synthetic local: key.
	resp, err := c.Get(srv.URL + "/projects")
	if err != nil {
		t.Fatalf("get /projects: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "github.com/grace-hopper/akari") {
		t.Fatalf("projects index missing the remote project, got:\n%s", body)
	}
	// "Folders without a git remote" was the old local-folder section's subtitle;
	// the folder names, paths, state tags, and synthetic key must all be gone too.
	// (">Sessions<" is avoided here: the sidebar nav link would match it.)
	for _, gone := range []string{
		"Folders without a git remote", "standalone", "orphaned",
		"scratch", "/home/grace/scratch", "deleted", "local:grace-laptop:",
	} {
		if strings.Contains(body, gone) {
			t.Fatalf("projects index should exclude local folders; found %q in:\n%s", gone, body)
		}
	}

	// Drilling into the standalone folder still shows its state tag and path: the
	// folder is off the index, not unreachable.
	var projID int64
	if err := st.Pool.QueryRow(ctx, "SELECT id FROM projects WHERE kind = 'standalone'").Scan(&projID); err != nil {
		t.Fatalf("find standalone project: %v", err)
	}
	resp, err = c.Get(fmt.Sprintf("%s/projects/%d", srv.URL, projID))
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	body = readBody(t, resp)
	for _, want := range []string{"standalone", "/home/grace/scratch"} {
		if !strings.Contains(body, want) {
			t.Fatalf("project page missing %q, got:\n%s", want, body)
		}
	}
}

func TestLoginPreservesNext(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	c := newClient(t)

	// Seed an account (first user, admin).
	if _, err := c.PostForm(srv.URL+"/register", url.Values{
		"username": {"ada"}, "password": {"lovelace-1843"},
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Log out so the next login starts clean.
	if _, err := c.PostForm(srv.URL+"/logout", url.Values{}); err != nil {
		t.Fatalf("logout: %v", err)
	}

	// The login page carries the next target as a hidden field.
	resp, err := c.Get(srv.URL + "/login?next=%2Faccount")
	if err != nil {
		t.Fatalf("login page: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `name="next" value="/account"`) {
		t.Fatalf("login page should carry next, got:\n%s", body)
	}

	// Stop following redirects so we can inspect the post-login Location.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err = c.PostForm(srv.URL+"/login", url.Values{
		"username": {"ada"}, "password": {"lovelace-1843"}, "next": {"/account"},
	})
	if err != nil {
		t.Fatalf("login post: %v", err)
	}
	if loc := resp.Header.Get("Location"); loc != "/account" {
		t.Fatalf("post-login redirect = %q, want /account", loc)
	}
	resp.Body.Close()
}

func TestPublicSessionFlow(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()
	c := newClient(t)

	// Seed an owner with one session carrying a searchable message.
	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: owner.ID, Agent: "claude", SourceSessionID: "sess-1",
		ProjectID: projectID, GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	sid := ann.SessionID
	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "Fix the secret login bug"},
		},
	}); err != nil {
		t.Fatalf("apply projection: %v", err)
	}

	// Log in as the owner.
	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}

	// Before publishing, an anonymous client cannot reach the session by id (it is
	// redirected to login) and there is no public link yet.
	anon := newClient(t)
	anon.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := anon.Get(srv.URL + fmt.Sprintf("/sessions/%d", sid))
	if err != nil {
		t.Fatalf("anon session by id: %v", err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("anon /sessions/%d status = %d, want 303 redirect", sid, resp.StatusCode)
	}
	resp.Body.Close()

	// Owner publishes the session.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err = c.PostForm(srv.URL+fmt.Sprintf("/sessions/%d/publish", sid), url.Values{})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("publish status = %d, want 303", resp.StatusCode)
	}
	resp.Body.Close()

	d, err := st.SessionDetailByID(ctx, sid)
	if err != nil || d.PublicID == nil {
		t.Fatalf("session not public after publish: err=%v publicID=%v", err, d.PublicID)
	}
	pid := *d.PublicID

	// An anonymous client can now read the public page and its content.
	anon.CheckRedirect = nil
	resp, err = anon.Get(srv.URL + "/s/" + pid)
	if err != nil {
		t.Fatalf("anon public view: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public view status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "Fix the secret login bug") || !strings.Contains(body, "Shared by grace") {
		t.Fatalf("public page missing content, got:\n%s", body)
	}
	// The public page must not expose the numeric session id, neither as a
	// /sessions/{id} path nor as a "#<id>" label.
	if strings.Contains(body, fmt.Sprintf("/sessions/%d", sid)) {
		t.Fatalf("public page leaked numeric session path, got:\n%s", body)
	}
	if strings.Contains(body, fmt.Sprintf("#%d", sid)) {
		t.Fatalf("public page leaked numeric session id label, got:\n%s", body)
	}

	// Owner unpublishes; the public link stops resolving.
	resp, err = c.PostForm(srv.URL+fmt.Sprintf("/sessions/%d/unpublish", sid), url.Values{})
	if err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	resp.Body.Close()
	anon.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err = anon.Get(srv.URL + "/s/" + pid)
	if err != nil {
		t.Fatalf("anon public view after unpublish: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("public view after unpublish status = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestOverviewRangeWindow drives the overview through its range query param: the
// default load marks the year window active, and ?range=90d marks the 90-day
// window instead (and not the default). This exercises handleOverview's ParseRange
// wiring end to end, the panel only renders its selector once there is usage data.
func TestOverviewRangeWindow(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()
	c := newClient(t)

	// Seed an owner, a project, a session, and one in-window usage event so the
	// overview has data and renders the usage panel (and thus the range selector).
	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: owner.ID, Agent: "claude", SourceSessionID: "sess-1",
		ProjectID: projectID, Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1, 'claude-opus-4-8', 100, 50, 1.0, now() - make_interval(days => 1), 'u1')`,
		ann.SessionID); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}

	// The default load opens on the year window.
	body := readBody(t, mustGet(t, c, srv.URL+"/"))
	if !strings.Contains(body, `class="seg active" hx-get="/?range=year"`) {
		t.Fatalf("default overview should mark the year window active, got:\n%s", body)
	}

	// ?range=90d moves the active window and leaves the default unmarked.
	body = readBody(t, mustGet(t, c, srv.URL+"/?range=90d"))
	if !strings.Contains(body, `class="seg active" hx-get="/?range=90d"`) {
		t.Fatalf("range=90d should mark the 90-day window active, got:\n%s", body)
	}
	if strings.Contains(body, `class="seg active" hx-get="/?range=year"`) {
		t.Fatalf("range=90d should not also mark the default window active, got:\n%s", body)
	}
}

// TestProjectPageRangeWindow drives the project page through its range query param
// and the htmx target gating. The project view reads like the overview, scoped to
// one project: it renders the calendar heatmap with a window selector pointed at
// the project's own path, the default load marks the 30-day window and ?range=90d
// moves it, and the two htmx callers split by target: a request for #usage gets the
// full usage panel (the range selector's swap), one for #session-list gets only the
// session list (the filter form's swap).
func TestProjectPageRangeWindow(t *testing.T) {
	t.Parallel()
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
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: owner.ID, Agent: "claude", SourceSessionID: "sess-1",
		ProjectID: projectID, Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1, 'claude-opus-4-8', 100, 50, 1.0, now() - make_interval(days => 1), 'u1')`,
		ann.SessionID); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	// A second session whose usage landed 60 days ago: outside the default 30-day
	// window, so it should drop out of the session list under that window and
	// reappear only when the window widens to all of history. The table is windowed
	// by usage date (it shares the panel's base), so the old session needs a dated
	// usage event in that window, not just an aged updated_at.
	annOld, err := st.Announce(ctx, store.AnnounceParams{
		UserID: owner.ID, Agent: "claude", SourceSessionID: "sess-old",
		ProjectID: projectID, Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce old: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1, 'claude-opus-4-8', 200, 100, 2.0, now() - make_interval(days => 60), 'u-old')`,
		annOld.SessionID); err != nil {
		t.Fatalf("seed old usage: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE sessions SET updated_at = now() - make_interval(days => 60) WHERE id = $1`,
		annOld.SessionID); err != nil {
		t.Fatalf("age old session: %v", err)
	}
	recentPath := fmt.Sprintf("/sessions/%d", ann.SessionID)
	oldPath := fmt.Sprintf("/sessions/%d", annOld.SessionID)

	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}

	base := fmt.Sprintf("/projects/%d", projectID)

	// The default load renders the heatmap (not the old line chart) and opens on the
	// year window (the shared default), with the selector refetching from the
	// project's own path.
	body := readBody(t, mustGet(t, c, srv.URL+base))
	if !strings.Contains(body, "data-heatmap") || strings.Contains(body, "data-chart-target") {
		t.Fatalf("project page should render the heatmap and no line chart, got:\n%s", body)
	}
	if !strings.Contains(body, `class="seg active" hx-get="`+base+`?range=year"`) {
		t.Fatalf("default project page should mark the year window active, got:\n%s", body)
	}

	// ?range=90d moves the active window and leaves the default unmarked.
	body = readBody(t, mustGet(t, c, srv.URL+base+"?range=90d"))
	if !strings.Contains(body, `class="seg active" hx-get="`+base+`?range=90d"`) {
		t.Fatalf("range=90d should mark the 90-day window active, got:\n%s", body)
	}
	if strings.Contains(body, `class="seg active" hx-get="`+base+`?range=year"`) {
		t.Fatalf("range=90d should not also mark the default window active, got:\n%s", body)
	}

	// The session list is windowed by the same range: under a 30-day window the
	// recent session shows and the 60-day-old one drops out; widening to all of
	// history brings it back.
	body = readBody(t, mustGet(t, c, srv.URL+base+"?range=30d"))
	if !strings.Contains(body, recentPath) {
		t.Fatalf("30-day window should list the recent session, got:\n%s", body)
	}
	if strings.Contains(body, oldPath) {
		t.Fatalf("30-day window should drop the 60-day-old session, got:\n%s", body)
	}
	body = readBody(t, mustGet(t, c, srv.URL+base+"?range=all"))
	if !strings.Contains(body, oldPath) {
		t.Fatalf("the all-history window should list the 60-day-old session, got:\n%s", body)
	}

	// The range selector and filter form both swap the whole #project-view, so the
	// usage panel and the session table re-scope together rather than drifting apart
	// (the panel narrowing with the rows under a filter is the point). The controls
	// target that region, and a swap of it carries both the panel and the list.
	if !strings.Contains(body, `id="project-view"`) {
		t.Fatalf("project page should wrap the panel and table in #project-view, got:\n%s", body)
	}
	if !strings.Contains(body, `hx-target="#project-view"`) || !strings.Contains(body, `hx-select="#project-view"`) {
		t.Fatalf("project controls should target #project-view, got:\n%s", body)
	}
	reqView, _ := http.NewRequest(http.MethodGet, srv.URL+base+"?range=all", nil)
	reqView.Header.Set("HX-Request", "true")
	reqView.Header.Set("HX-Target", "project-view")
	body = readBody(t, mustDo(t, c, reqView))
	if !strings.Contains(body, "data-heatmap") || !strings.Contains(body, `id="session-list"`) {
		t.Fatalf("a #project-view swap should carry both the usage panel and the session list, got:\n%s", body)
	}
}

// TestSessionsFeedRangeWindow drives the sessions feed's ?range drill-down bound: a bounded key
// (30d) drops a session whose only activity predates the window, while the bare feed and an
// unknown range value both stay all-history and list it. This pins handleSessions' range parse:
// web.RangeBounds is the whitelist, so a bounded key sets SessionFilter.Since and anything else
// (absent, "all", or a hand-typed junk value) leaves the feed unbounded rather than falling to
// ParseRange's trailing-year default. The active-range chip renders for the bounded case so the
// reader sees the feed is scoped.
func TestSessionsFeedRangeWindow(t *testing.T) {
	t.Parallel()
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
	// A recent session (started yesterday) and an old one (started 60 days ago), so a 30-day window
	// keeps the recent one and drops the old one. The feed's ?range drill-down windows on started_at,
	// matching the Insights/People bars it arrives from, so this stamps the recent session's
	// started_at inside the window and ages the old one's out of it.
	annNew, err := st.Announce(ctx, store.AnnounceParams{
		UserID: owner.ID, Agent: "claude", SourceSessionID: "sess-new",
		ProjectID: projectID, Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce new: %v", err)
	}
	annOld, err := st.Announce(ctx, store.AnnounceParams{
		UserID: owner.ID, Agent: "claude", SourceSessionID: "sess-old",
		ProjectID: projectID, Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce old: %v", err)
	}
	// Stamp a message on each so they clear the feed's default empty-session hide (a bare
	// announce parses no message), then set their started_at so the 30-day window keeps one
	// and drops the other.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE sessions SET started_at = now() - make_interval(days => 1), message_count = 1 WHERE id = $1`,
		annNew.SessionID); err != nil {
		t.Fatalf("date new session: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE sessions SET started_at = now() - make_interval(days => 60), message_count = 1 WHERE id = $1`,
		annOld.SessionID); err != nil {
		t.Fatalf("age old session: %v", err)
	}
	recentPath := fmt.Sprintf("/sessions/%d", annNew.SessionID)
	oldPath := fmt.Sprintf("/sessions/%d", annOld.SessionID)

	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}

	// A bounded window (30d) keeps the recent session and drops the 60-day-old one, and the feed
	// shows the active-range chip so the scope is visible and removable.
	body := readBody(t, mustGet(t, c, srv.URL+"/sessions?range=30d"))
	if !strings.Contains(body, recentPath) {
		t.Fatalf("range=30d should list the recent session, got:\n%s", body)
	}
	if strings.Contains(body, oldPath) {
		t.Fatalf("range=30d should drop the 60-day-old session, got:\n%s", body)
	}
	if !strings.Contains(body, `<span class="fchip-k">range</span>`) {
		t.Fatalf("range=30d feed should show the active-range chip, got:\n%s", body)
	}

	// The bare feed is unbounded (all-history), so it lists the old session too, and shows no
	// range chip.
	body = readBody(t, mustGet(t, c, srv.URL+"/sessions"))
	if !strings.Contains(body, oldPath) {
		t.Fatalf("the bare feed should be all-history and list the old session, got:\n%s", body)
	}
	if strings.Contains(body, `<span class="fchip-k">range</span>`) {
		t.Fatalf("the unbounded feed should show no range chip, got:\n%s", body)
	}

	// An unknown range value is not a bound: it reads as all-history rather than falling to a
	// trailing-year default, so the old session still lists and no chip appears.
	body = readBody(t, mustGet(t, c, srv.URL+"/sessions?range=bogus"))
	if !strings.Contains(body, oldPath) {
		t.Fatalf("an unknown range value should leave the feed unbounded, got:\n%s", body)
	}
	if strings.Contains(body, `<span class="fchip-k">range</span>`) {
		t.Fatalf("an unknown range value should show no range chip, got:\n%s", body)
	}
}

// TestSessionsFeedGradeOutcomeParams drives handleSessions' ?grade and ?outcome whitelist
// validation: a known grade or outcome renders the feed (200), and an unrecognized value of
// either is rejected as a bad request rather than silently falling through to the unfiltered
// list, matching the project-filter precedent the handler already applies for ?project. The
// handler validates both params through web.IsGrade and web.IsOutcome directly, the same
// functions the drill-through links themselves are built to satisfy, so a valid case here
// also pins that the two ends cannot disagree.
func TestSessionsFeedGradeOutcomeParams(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()
	c := newClient(t)

	if _, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}

	cases := []struct {
		name       string
		query      string
		wantStatus int
	}{
		{"valid grade", "grade=A", http.StatusOK},
		{"valid unscored grade", "grade=" + web.UnscoredKey, http.StatusOK},
		{"valid outcome", "outcome=completed", http.StatusOK},
		{"invalid grade", "grade=bogus", http.StatusBadRequest},
		{"invalid outcome", "outcome=bogus", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := mustGet(t, c, srv.URL+"/sessions?"+tc.query)
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("GET /sessions?%s = %d, want %d", tc.query, resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

// TestOverviewUserFilter drives the overview's per-user scope end to end: an
// unscoped load aggregates every user (both agents show in the breakdown) and
// lists each account as a filter option; ?user=<id> narrows the analytics to that
// user's sessions (the other user's agent drops out), marks their checkbox, and
// the range buttons carry the selection forward.
func TestOverviewUserFilter(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()
	c := newClient(t)

	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// A second account, inserted directly (registration past the first user is
	// invite-gated, which this test does not need to exercise).
	var adaID int64
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO users (username, password_hash, is_admin) VALUES ('ada', 'x', FALSE) RETURNING id`).Scan(&adaID); err != nil {
		t.Fatalf("seed ada: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	// grace runs claude, ada runs codex; each gets one in-window usage event so the
	// by-agent breakdown carries both agents in the unscoped view.
	seed := func(userID int64, agent, src, model string) {
		ann, err := st.Announce(ctx, store.AnnounceParams{
			UserID: userID, Agent: agent, SourceSessionID: src,
			ProjectID: projectID, Cwd: "/home/x/akari", Machine: "laptop",
		})
		if err != nil {
			t.Fatalf("announce %s: %v", src, err)
		}
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cost_usd, occurred_at, dedup_key)
			 VALUES ($1, $2, 100, 50, 1.0, now() - make_interval(days => 1), $3)`,
			ann.SessionID, model, src+"-u"); err != nil {
			t.Fatalf("seed usage %s: %v", src, err)
		}
	}
	seed(owner.ID, "claude", "sess-grace", "claude-opus-4-8")
	seed(adaID, "codex", "sess-ada", "gpt-5.5")

	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}

	// Unscoped: both users are offered, the collapsed control reads All Users, and
	// codex (ada's agent) appears in the breakdown alongside claude.
	body := readBody(t, mustGet(t, c, srv.URL+"/"))
	for _, want := range []string{
		fmt.Sprintf(`name="user" value="%d"`, owner.ID),
		fmt.Sprintf(`name="user" value="%d"`, adaID),
		`class="userfilter-all">All Users</span>`,
		`>codex</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("unscoped overview missing %q, got:\n%s", want, body)
		}
	}

	// Scoped to grace: her checkbox is checked, ada's codex usage drops out of the
	// breakdown, and the range buttons carry ?user=<grace> forward.
	body = readBody(t, mustGet(t, c, srv.URL+fmt.Sprintf("/?user=%d", owner.ID)))
	if !strings.Contains(body, fmt.Sprintf(`value="%d" checked`, owner.ID)) {
		t.Fatalf("grace scope should check her box, got:\n%s", body)
	}
	if strings.Contains(body, `>codex</span>`) {
		t.Fatalf("grace scope should exclude ada's codex usage, got:\n%s", body)
	}
	if !strings.Contains(body, fmt.Sprintf(`hx-get="/?range=30d&amp;user=%d"`, owner.ID)) {
		t.Fatalf("range buttons should carry the user scope, got:\n%s", body)
	}
}

// TestSessionPageDuplicateIDChip drives the real session page over HTTP and confirms
// the duplicate-id chip renders from store data: a session whose transcript repeated
// a tool_use id shows the warning, computed by handleSessionPage through
// DuplicateCallUIDCount rather than from hand-built view models.
func TestSessionPageDuplicateIDChip(t *testing.T) {
	t.Parallel()
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
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: owner.ID, Agent: "claude", SourceSessionID: "sess-dup",
		ProjectID: projectID, GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	sid := ann.SessionID

	// Two assistant turns whose tool calls share id "toolu_dup": the replayed turn.
	if err := st.ApplyProjectionDelta(ctx, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "assistant", Content: "first", HasToolUse: true},
			{Ordinal: 1, Role: "assistant", Content: "replay", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			{MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", CallUID: "toolu_dup"},
			{MessageOrdinal: 1, CallIndex: 0, ToolName: "Read", CallUID: "toolu_dup"},
		},
	}); err != nil {
		t.Fatalf("apply projection: %v", err)
	}

	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}

	resp, err := c.Get(srv.URL + fmt.Sprintf("/sessions/%d", sid))
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session page status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body, "1 duplicate id") {
		t.Fatalf("session page should show the duplicate-id chip, got:\n%s", body)
	}
}

// TestPublicOverviewFlow drives a user's public overview end to end: it is
// unreachable before publishing, the account Publicity control publishes it and
// surfaces the share link (and the signed-in overview gains its badge), an
// anonymous viewer then reads only that user's aggregate usage (never another
// account's, never a session), and making it private 404s the link while a
// re-publish restores the same URL.
func TestPublicOverviewFlow(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()
	c := newClient(t)

	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// A second account whose usage must never leak onto grace's public overview.
	var adaID int64
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO users (username, password_hash, is_admin) VALUES ('ada', 'x', FALSE) RETURNING id`).Scan(&adaID); err != nil {
		t.Fatalf("seed ada: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	// grace runs claude, ada runs codex; one in-window usage event each.
	seed := func(userID int64, agent, src, model string) int64 {
		ann, err := st.Announce(ctx, store.AnnounceParams{
			UserID: userID, Agent: agent, SourceSessionID: src,
			ProjectID: projectID, Cwd: "/home/x/akari", Machine: "laptop",
		})
		if err != nil {
			t.Fatalf("announce %s: %v", src, err)
		}
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cost_usd, occurred_at, dedup_key)
			 VALUES ($1, $2, 100, 50, 1.0, now() - make_interval(days => 1), $3)`,
			ann.SessionID, model, src+"-u"); err != nil {
			t.Fatalf("seed usage %s: %v", src, err)
		}
		return ann.SessionID
	}
	graceSession := seed(owner.ID, "claude", "sess-grace", "claude-opus-4-8")
	seed(adaID, "codex", "sess-ada", "gpt-5.5")

	const pubPath = "/u/grace"

	// Before publishing, the username 404s (the public page never redirects to
	// login).
	anon := newClient(t)
	anon.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp := mustGet(t, anon, srv.URL+pubPath)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("anon %s before publish = %d, want 404", pubPath, resp.StatusCode)
	}
	resp.Body.Close()
	anon.CheckRedirect = nil

	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}

	// Before publishing, the account page offers the publish control, not a link.
	body := readBody(t, mustGet(t, c, srv.URL+"/account"))
	if !strings.Contains(body, "Publicity") || !strings.Contains(body, "Make overview public") {
		t.Fatalf("account page should offer the publicity control, got:\n%s", body)
	}
	// The signed-in overview carries no public badge while private.
	body = readBody(t, mustGet(t, c, srv.URL+"/"))
	if strings.Contains(body, "View public page") {
		t.Fatalf("overview should not show the public badge before publishing, got:\n%s", body)
	}

	// Publish via the account control.
	if _, err := c.PostForm(srv.URL+"/account/overview/publish", url.Values{}); err != nil {
		t.Fatalf("publish overview: %v", err)
	}
	if u, err := st.UserByID(ctx, owner.ID); err != nil || !u.OverviewPublic {
		t.Fatalf("account not public after publish: err=%v public=%v", err, u.OverviewPublic)
	}

	// The account page now shows the username link and the make-private control; the
	// signed-in overview gains the badge linking to the public page.
	body = readBody(t, mustGet(t, c, srv.URL+"/account"))
	if !strings.Contains(body, pubPath) || !strings.Contains(body, "Make private") {
		t.Fatalf("account page should show the username link and make-private control, got:\n%s", body)
	}
	body = readBody(t, mustGet(t, c, srv.URL+"/"))
	if !strings.Contains(body, "View public page") || !strings.Contains(body, pubPath) {
		t.Fatalf("overview should show the public badge after publishing, got:\n%s", body)
	}

	// An anonymous viewer reads grace's aggregate usage: her agent (claude) and her
	// username, but never ada's codex usage and never a session link.
	resp = mustGet(t, anon, srv.URL+pubPath)
	body = readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anon public overview status = %d, want 200", resp.StatusCode)
	}
	for _, want := range []string{"grace", ">claude</span>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("public overview missing %q, got:\n%s", want, body)
		}
	}
	if strings.Contains(body, ">codex</span>") {
		t.Fatalf("public overview leaked another user's usage (codex), got:\n%s", body)
	}
	// The public overview is aggregate only: it must expose no session, neither
	// grace's own session path nor the per-user filter that names other accounts.
	if strings.Contains(body, fmt.Sprintf("/sessions/%d", graceSession)) {
		t.Fatalf("public overview leaked a session path, got:\n%s", body)
	}
	if strings.Contains(body, fmt.Sprintf(`name="user" value="%d"`, adaID)) {
		t.Fatalf("public overview leaked the per-user filter, got:\n%s", body)
	}
	// Its range buttons refetch the public path, not the authed overview.
	if !strings.Contains(body, `hx-get="`+pubPath+`?range=`) {
		t.Fatalf("public overview range buttons should target the public path, got:\n%s", body)
	}

	// Another account's overview is independent: ada never published, so /u/ada
	// 404s even while grace's is public.
	anon.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp = mustGet(t, anon, srv.URL+"/u/ada")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unpublished /u/ada = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// Make private: the link 404s.
	anon.CheckRedirect = nil
	if _, err := c.PostForm(srv.URL+"/account/overview/unpublish", url.Values{}); err != nil {
		t.Fatalf("unpublish overview: %v", err)
	}
	anon.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp = mustGet(t, anon, srv.URL+pubPath)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("public overview after make-private = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	anon.CheckRedirect = nil

	// Re-publishing brings the same /u/<username> back.
	if _, err := c.PostForm(srv.URL+"/account/overview/publish", url.Values{}); err != nil {
		t.Fatalf("re-publish overview: %v", err)
	}
	resp = mustGet(t, anon, srv.URL+pubPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public overview after re-publish = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestPublicOverviewOGImage drives the Open Graph preview card end to end: the
// public page advertises it via og:image meta tags, the /og.png route renders a
// valid 1200x630 PNG on demand and caches it, a repeat fetch within the TTL is
// served from the cache (not re-rendered), an expired card re-renders, and making
// the overview private 404s the card just as it does the page.
func TestPublicOverviewOGImage(t *testing.T) {
	t.Parallel()
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
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: owner.ID, Agent: "claude", SourceSessionID: "sess-grace",
		ProjectID: projectID, Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1, 'claude-opus-4-8', 100, 50, 1.0, now() - make_interval(days => 1), 'u1')`,
		ann.SessionID); err != nil {
		t.Fatalf("seed usage: %v", err)
	}

	anon := newClient(t)
	anon.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	// Before publishing, the card 404s (the page does too).
	resp := mustGet(t, anon, srv.URL+"/u/grace/og.png")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("og.png before publish = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	// Publish through the account control. Publishing does not render the card; the
	// first fetch of /og.png does.
	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := c.PostForm(srv.URL+"/account/overview/publish", url.Values{}); err != nil {
		t.Fatalf("publish overview: %v", err)
	}

	// Publishing must not render a card synchronously: it is rendered lazily on the
	// first /og.png fetch. If publish-time rendering were reintroduced, this would
	// fail (and the "first fetch renders" assertion below would still pass, so this
	// guards the behavior change directly).
	if _, err := st.OverviewOGImage(ctx, owner.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("card cached right after publish (err = %v), want ErrNotFound (no publish-time render)", err)
	}

	// The public page advertises the card via Open Graph meta tags (the tags do not
	// depend on a card being cached yet: they name the URL the crawler will fetch).
	body := readBody(t, mustGet(t, anon, srv.URL+"/u/grace"))
	for _, want := range []string{
		`property="og:image" content="`,
		`/u/grace/og.png"`,
		`name="twitter:card" content="summary_large_image"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("public overview missing OG tag %q, got:\n%s", want, body)
		}
	}

	// Under a narrower ?range the page totals no longer match the year-window card,
	// so the card must not be advertised there (it would unfurl a mismatched figure);
	// the page still renders fine, just without og:image.
	ranged := readBody(t, mustGet(t, anon, srv.URL+"/u/grace?range=7d"))
	if strings.Contains(ranged, "og:image") {
		t.Fatalf("non-default range page must not advertise the card, got:\n%s", ranged)
	}

	// The first fetch renders the card on demand: a valid, correctly sized PNG served
	// as an image.
	if b := fetchPNG(t, anon, srv.URL+"/u/grace/og.png"); b.Dx() != 1200 || b.Dy() != 630 {
		t.Fatalf("rendered og.png size = %dx%d, want 1200x630", b.Dx(), b.Dy())
	}

	// A repeat fetch within the TTL is served from the cache, not re-rendered. Prove
	// it by overwriting the cached bytes with a sentinel: a cache hit returns the
	// sentinel verbatim, while a re-render would overwrite it with a real PNG.
	sentinel := []byte("cached-sentinel-not-a-real-png")
	if _, err := st.PutOverviewOGImage(ctx, owner.ID, sentinel, time.Now()); err != nil {
		t.Fatalf("seed sentinel card: %v", err)
	}
	resp = mustGet(t, anon, srv.URL+"/u/grace/og.png")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cached og.png = %d, want 200", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read cached og.png: %v", err)
	}
	if !bytes.Equal(raw, sentinel) {
		t.Fatalf("fresh cache should serve the cached bytes unchanged, got %d bytes", len(raw))
	}

	// Age the cached card past the TTL; the next fetch re-renders it (the sentinel is
	// replaced with a real PNG again).
	if _, err := st.Pool.Exec(ctx,
		`UPDATE overview_og_images SET generated_at = now() - make_interval(hours => 2) WHERE user_id = $1`,
		owner.ID); err != nil {
		t.Fatalf("age cached card: %v", err)
	}
	if b := fetchPNG(t, anon, srv.URL+"/u/grace/og.png"); b.Dx() != 1200 || b.Dy() != 630 {
		t.Fatalf("re-rendered og.png size = %dx%d, want 1200x630", b.Dx(), b.Dy())
	}

	// Making the overview private 404s the card, matching the page.
	if _, err := c.PostForm(srv.URL+"/account/overview/unpublish", url.Values{}); err != nil {
		t.Fatalf("unpublish overview: %v", err)
	}
	resp = mustGet(t, anon, srv.URL+"/u/grace/og.png")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("og.png after make-private = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestLandingOGImage drives the homepage preview card: the logged-out root at "/"
// advertises it via og:image (a URL ending in /og.png) with the large-image
// Twitter card, and /og.png serves a valid 1200x630 image/png with the
// deploy-lifetime Cache-Control. Unlike the overview card, the landing card
// carries no user data, so it needs no published account or usage fixtures beyond
// the test server itself (which gates on AKARI_TEST_DATABASE_URL like its
// neighbors).
func TestLandingOGImage(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)

	anon := newClient(t)
	anon.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	// The logged-out root advertises the card via Open Graph and the large-image
	// Twitter card, naming the /og.png URL the crawler will fetch. The title and
	// description assert against the ogimage package's canonical landing copy
	// (the strings the card itself draws), pinning the derivation in handleRoot:
	// the meta tags cannot drift from the image without failing here.
	wantTitle := "akari · " + strings.ToLower(strings.TrimSuffix(ogimage.LandingHeadline, "."))
	body := readBody(t, mustGet(t, anon, srv.URL+"/"))
	for _, want := range []string{
		`property="og:image" content="`,
		`/og.png"`,
		`name="twitter:card" content="summary_large_image"`,
		`property="og:title" content="` + wantTitle + `"`,
		`property="og:description" content="` + ogimage.LandingSubline + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("landing root missing OG tag %q, got:\n%s", want, body)
		}
	}

	// The card itself: a valid, correctly sized PNG.
	if b := fetchPNG(t, anon, srv.URL+"/og.png"); b.Dx() != 1200 || b.Dy() != 630 {
		t.Fatalf("landing og.png size = %dx%d, want 1200x630", b.Dx(), b.Dy())
	}

	// It is static per binary, so it carries a deploy-lifetime Cache-Control rather
	// than the overview card's short TTL.
	resp := mustGet(t, anon, srv.URL+"/og.png")
	resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "public, max-age=86400" {
		t.Fatalf("landing og.png Cache-Control = %q, want %q", got, "public, max-age=86400")
	}
}

// fetchPNG GETs a URL, asserts a 200 image/png, and returns the decoded image's
// bounds so a caller can check the card's dimensions.
func fetchPNG(t *testing.T, c *http.Client, url string) image.Rectangle {
	t.Helper()
	resp := mustGet(t, c, url)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("GET %s content-type = %q, want image/png", url, ct)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return img.Bounds()
}

// TestOGImageDuringReparse guards the on-demand render's reparse gate: rendering a
// card while a reparse rebuilds the projection must not serve a card from a
// half-rebuilt aggregate. It holds the real reparse advisory lock (as a live reparse
// does for its whole run) so ogimage.Generate takes its abort path. With a cold
// cache the request 404s (nothing good to serve yet); once the reparse clears, the
// next request renders and serves the card.
func TestOGImageDuringReparse(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()
	c := newClient(t)

	if _, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := c.PostForm(srv.URL+"/account/overview/publish", url.Values{}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	anon := newClient(t)
	anon.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	// Hold the reparse advisory lock, standing in for a running reparse: an on-demand
	// render against a cold cache aborts, so /og.png 404s rather than caching a
	// half-rebuilt aggregate.
	lock, ok, err := st.AcquireReparseLock(ctx)
	if err != nil || !ok {
		t.Fatalf("acquire reparse lock: ok=%v err=%v", ok, err)
	}
	resp := mustGet(t, anon, srv.URL+"/u/grace/og.png")
	if resp.StatusCode != http.StatusNotFound {
		lock.Release(ctx)
		t.Fatalf("og.png during reparse (cold cache) = %d, want 404 (render skipped)", resp.StatusCode)
	}
	resp.Body.Close()

	// The aborted render must not have cached anything: a half-rebuilt aggregate is
	// never stored, so the cache is still empty (not a bad card waiting to be served).
	owner, err := st.UserByUsername(ctx, "grace")
	if err != nil {
		t.Fatalf("lookup grace: %v", err)
	}
	if _, err := st.OverviewOGImage(ctx, owner.ID); !errors.Is(err, store.ErrNotFound) {
		lock.Release(ctx)
		t.Fatalf("card cached during aborted render (err = %v), want ErrNotFound (nothing stored)", err)
	}

	// With the lock cleared, the next fetch renders the card.
	lock.Release(ctx)
	resp = mustGet(t, anon, srv.URL+"/u/grace/og.png")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("og.png after reparse cleared = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestOGImageServesStaleCardDuringReparse guards the warm-cache half of the reparse
// gate: when a reparse blocks a fresh render but a previously rendered card is still
// in the cache (even one past its TTL), the handler serves that last good card rather
// than 404ing. A crawler unfurling the link during a reparse gets the old picture,
// not a broken preview, and the card refreshes once the reparse clears.
func TestOGImageServesStaleCardDuringReparse(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()
	c := newClient(t)

	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := c.PostForm(srv.URL+"/account/overview/publish", url.Values{}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Seed a stale cached card (stamped two hours back, well past the 1h TTL) with a
	// sentinel body so a cache hit is unmistakable. A fresh render would replace it,
	// but the held reparse lock blocks that, so the stale bytes must come back.
	stale := []byte("stale-card-served-during-reparse")
	if _, err := st.PutOverviewOGImage(ctx, owner.ID, stale, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatalf("seed stale card: %v", err)
	}

	lock, ok, err := st.AcquireReparseLock(ctx)
	if err != nil || !ok {
		t.Fatalf("acquire reparse lock: ok=%v err=%v", ok, err)
	}
	defer lock.Release(ctx)

	anon := newClient(t)
	resp := mustGet(t, anon, srv.URL+"/u/grace/og.png")
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("og.png with a stale card during reparse = %d, want 200 (serve the stale card)", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read stale og.png: %v", err)
	}
	if !bytes.Equal(raw, stale) {
		t.Fatalf("reparse fallback should serve the stale card verbatim, got %d bytes", len(raw))
	}
}

// TestOGImageCacheControlHonorsTTL pins the served Cache-Control window to the
// configured TTL: a crawler's max-age matches how long the server itself treats the
// cached card as fresh, so repeat unfurls stay off the render path for the same span
// rather than a hardcoded default.
func TestOGImageCacheControlHonorsTTL(t *testing.T) {
	t.Parallel()
	const ttl = 15 * time.Minute
	st := storetest.NewStore(t)
	rp := reparse.New(context.Background(), st)
	srv := httptest.NewServer(New(st, config.Server{OGCacheTTL: ttl}, rp).Routes())
	t.Cleanup(srv.Close)

	ctx := context.Background()
	c := newClient(t)
	if _, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := c.PostForm(srv.URL+"/account/overview/publish", url.Values{}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	anon := newClient(t)
	resp := mustGet(t, anon, srv.URL+"/u/grace/og.png")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("og.png = %d, want 200", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Cache-Control"), fmt.Sprintf("public, max-age=%d", int(ttl.Seconds())); got != want {
		t.Fatalf("Cache-Control = %q, want %q (mirrors the configured TTL)", got, want)
	}
}

// TestOGCardWindowReconcilesWithDefaultRange pins the card's analytics window to
// the public overview's default range on BOTH bounds. The card is generated from a
// fixed trailing-year window bounded at the end of today and is advertised only on
// the default-range page; the handler feeds that page the same bounds, so a
// future-dated event cannot land in the page total while the card omits it. Both
// halves are pinned here so a change to either bound that breaks the match fails
// loudly.
func TestOGCardWindowReconcilesWithDefaultRange(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	// Lower bound: the card's DefaultSince equals the default range's Since.
	if card, page := ogimage.DefaultSince(now), web.RangeSince(web.DefaultRange, now); !card.Equal(page) {
		t.Fatalf("card Since %v != default page Since %v", card, page)
	}
	// Upper bound: both the card and the page cut off at the end of today, so the
	// handler must apply ogimage.DefaultUntil to the page (see handlePublicOverview).
	// DefaultUntil is the exclusive start of tomorrow, UTC.
	if got, want := ogimage.DefaultUntil(now), now.UTC().Truncate(24*time.Hour).AddDate(0, 0, 1); !got.Equal(want) {
		t.Fatalf("DefaultUntil(%v) = %v, want end of today %v", now, got, want)
	}
}

// TestPublicOverviewPublishRequiresAuth confirms the publicity toggles are gated:
// a logged-out client cannot publish or unpublish another account's overview, so
// the public page is opt-in by its owner alone and not flippable by anyone who
// finds the route.
func TestPublicOverviewPublishRequiresAuth(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()

	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}

	// No credential: the full-scope guard rejects the POST and nothing toggles.
	anon := newClient(t)
	anon.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, path := range []string{"/account/overview/publish", "/account/overview/unpublish"} {
		resp, err := anon.PostForm(srv.URL+path, url.Values{})
		if err != nil {
			t.Fatalf("anon post %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anon POST %s = %d, want 401", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if u, _ := st.UserByID(ctx, owner.ID); u.OverviewPublic {
		t.Fatalf("overview public after rejected anon publish, want still private")
	}
}

// TestSessionsSearchAndPaging exercises the global Sessions surface end to end
// over HTTP: a content search narrows the feed and renders the match in <mark>
// (escaped, from the template), a query like <script> renders escaped rather than
// injected, the empty toggle hides zero-message sessions by default and shows them
// on ?empty=1, and the limit param is clamped.
func TestSessionsSearchAndPaging(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()

	// Register the first account over HTTP so the cookie-carrying client is signed in
	// (the first account becomes admin, no invite needed); the sessions are seeded
	// under it so the reader can view them.
	c := newClient(t)
	if _, err := c.PostForm(srv.URL+"/register", url.Values{"username": {"grace"}, "password": {"hopper-1906"}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	owner, err := st.UserByUsername(ctx, "grace")
	if err != nil {
		t.Fatalf("lookup owner: %v", err)
	}
	proj, err := st.UpsertProject(ctx, "github.com/x/a", "github.com", "x", "a", "a", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	// Seed sessions with messages directly (bypassing ingest), bumping message_count
	// so the default empty-hide keeps them. One empty session (no message) exercises
	// the toggle.
	seedSess := func(src string) int64 {
		t.Helper()
		var id int64
		if err := st.Pool.QueryRow(ctx,
			`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine)
			 VALUES ($1,$2,'claude',$3,'box') RETURNING id`, owner.ID, proj, src).Scan(&id); err != nil {
			t.Fatalf("seed session %s: %v", src, err)
		}
		return id
	}
	seedMsg := func(sid int64, ord int, role, content string) {
		t.Helper()
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO messages (session_id, ordinal, role, content) VALUES ($1,$2,$3,$4)`,
			sid, ord, role, content); err != nil {
			t.Fatalf("seed message: %v", err)
		}
		if _, err := st.Pool.Exec(ctx,
			`UPDATE sessions SET message_count = message_count + 1 WHERE id = $1`, sid); err != nil {
			t.Fatalf("bump count: %v", err)
		}
	}
	hit := seedSess("hit")
	seedMsg(hit, 0, "user", "Refactor the pricing reconcile pass, please.")
	xss := seedSess("xss")
	seedMsg(xss, 0, "user", "Look at <script>danger</script> in the pricing table.")
	other := seedSess("other")
	seedMsg(other, 0, "user", "Unrelated conversation about the weather.")
	seedSess("empty") // no message: message_count stays 0

	// A content search narrows to the two pricing sessions and renders a <mark>.
	body := readBody(t, mustGet(t, c, srv.URL+"/sessions?q=pricing"))
	if !strings.Contains(body, "<mark>") {
		t.Errorf("search should render a highlighted match, got:\n%s", body)
	}
	if strings.Contains(body, "weather") {
		t.Error("search 'pricing' should not include the unrelated session")
	}
	// The search chip is present and removable.
	if !strings.Contains(body, `<span class="fchip-k">search</span>`) {
		t.Error("an active search should show a removable chip")
	}

	// A query containing markup renders escaped, never injected: the raw <script>
	// from the message must not appear as an element.
	xssBody := readBody(t, mustGet(t, c, srv.URL+"/sessions?q=script"))
	if strings.Contains(xssBody, "<script>danger</script>") {
		t.Errorf("message content must be escaped, not injected, got:\n%s", xssBody)
	}
	if !strings.Contains(xssBody, "&lt;script&gt;danger&lt;/script&gt;") {
		t.Errorf("message content should render as escaped text, got:\n%s", xssBody)
	}

	// A query that is itself markup is escaped in the chip (a text node), not run.
	tagBody := readBody(t, mustGet(t, c, srv.URL+"/sessions?q=%3Cscript%3E"))
	if strings.Contains(tagBody, "<script>") && !strings.Contains(tagBody, "&lt;script&gt;") {
		t.Errorf("a <script> query must render escaped, got:\n%s", tagBody)
	}

	// Default hides the empty session: its "empty" source-derived row is absent, and
	// the footer offers to show it.
	def := readBody(t, mustGet(t, c, srv.URL+"/sessions"))
	if !strings.Contains(def, "empty hidden") {
		t.Errorf("default feed should offer to show hidden empties, got footer-less:\n%s", def)
	}
	// ?empty=1 includes it and the toggle flips to "showing empty".
	withEmpty := readBody(t, mustGet(t, c, srv.URL+"/sessions?empty=1"))
	if !strings.Contains(withEmpty, "showing empty") {
		t.Errorf("empty=1 should read 'showing empty', got:\n%s", withEmpty)
	}
}

// TestSessionsLimitClamp asserts the limit param is clamped to the store's window
// and that an over-cap request does not error.
func TestSessionsLimitClamp(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	c := newClient(t)
	if _, err := c.PostForm(srv.URL+"/register", url.Values{"username": {"grace"}, "password": {"hopper-1906"}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	// A wildly over-cap limit and a garbage limit both render a 200, not a 500.
	for _, q := range []string{"?limit=99999", "?limit=abc", "?limit=-5"} {
		resp := mustGet(t, c, srv.URL+"/sessions"+q)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("/sessions%s = %d, want 200 (limit should clamp, not error)", q, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"":                  "/",
		"/projects/1":       "/projects/1",
		"//evil.example":    "/",
		"https://evil/x":    "/",
		"/sessions?q=a":     "/sessions?q=a",
		"javascript:alert1": "/",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func mustGet(t *testing.T, c *http.Client, url string) *http.Response {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	return resp
}

// mustDo runs a prepared request (used to set htmx headers) through the cookie-
// carrying client, failing the test on a transport error.
func mustDo(t *testing.T, c *http.Client, req *http.Request) *http.Response {
	t.Helper()
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do %s: %v", req.URL, err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			break
		}
	}
	return string(b)
}
