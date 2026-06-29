package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
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
	st := storetest.NewStore(t)
	srv := httptest.NewServer(New(st, config.Server{}).Routes())
	t.Cleanup(srv.Close)
	return srv, st
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

	// An unauthenticated read page redirects to login.
	resp, err := c.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Log in") {
		t.Fatalf("unauthenticated / should land on login page, got:\n%s", body)
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

	// Logout clears the session; the root redirects to login again.
	resp, err = c.PostForm(srv.URL+"/logout", url.Values{})
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	body = readBody(t, resp)
	if !strings.Contains(body, "Log in") {
		t.Fatalf("after logout / should be login page, got:\n%s", body)
	}
}

// TestStandaloneOrphanedIndex drives the real ingest endpoint with a non-remote
// kind and confirms the index renders standalone and orphaned folders in their
// own "Sessions" section, tagged and labeled by folder, and that drilling into
// one shows its state and path.
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

	announce := func(kind, source, cwd string) {
		t.Helper()
		body, _ := json.Marshal(map[string]string{
			"agent": "claude", "source_session_id": source, "kind": kind,
			"cwd": cwd, "machine": "grace-laptop",
		})
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

	announce("standalone", "sess-standalone", "/home/grace/scratch")
	announce("orphaned", "sess-orphaned", "/home/grace/deleted")

	// The projects index shows a Sessions section with both states tagged and the
	// folder name and path rendered (not the synthetic local: key).
	resp, err := c.Get(srv.URL + "/projects")
	if err != nil {
		t.Fatalf("get /projects: %v", err)
	}
	body := readBody(t, resp)
	for _, want := range []string{
		">Sessions<", "standalone", "orphaned", "scratch", "/home/grace/scratch", "deleted",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("projects index missing %q, got:\n%s", want, body)
		}
	}
	if strings.Contains(body, "local:grace-laptop:") {
		t.Fatalf("projects index leaked the synthetic local key, got:\n%s", body)
	}

	// Drilling into the standalone folder shows its state tag and path.
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
// default load marks the 30-day window active, and ?range=90d marks the 90-day
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

	// The default load opens on the 30-day window.
	body := readBody(t, mustGet(t, c, srv.URL+"/"))
	if !strings.Contains(body, `class="seg active" hx-get="/?range=30d"`) {
		t.Fatalf("default overview should mark the 30-day window active, got:\n%s", body)
	}

	// ?range=90d moves the active window and leaves the default unmarked.
	body = readBody(t, mustGet(t, c, srv.URL+"/?range=90d"))
	if !strings.Contains(body, `class="seg active" hx-get="/?range=90d"`) {
		t.Fatalf("range=90d should mark the 90-day window active, got:\n%s", body)
	}
	if strings.Contains(body, `class="seg active" hx-get="/?range=30d"`) {
		t.Fatalf("range=90d should not also mark the default window active, got:\n%s", body)
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
