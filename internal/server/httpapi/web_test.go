package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/migrations"
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

// newTestServer brings up a full Routes() handler backed by a freshly migrated
// test database, returning the server and its store. It is skipped unless
// AKARI_TEST_DATABASE_URL is set.
func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	dburl := os.Getenv("AKARI_TEST_DATABASE_URL")
	if dburl == "" {
		t.Skip("set AKARI_TEST_DATABASE_URL to run web integration tests")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dburl)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for _, q := range []string{"DROP SCHEMA public CASCADE", "CREATE SCHEMA public"} {
		if _, err := st.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("reset schema (%s): %v", q, err)
		}
	}
	if err := st.Migrate(ctx, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	srv := httptest.NewServer(New(st, config.Server{}).Routes())
	t.Cleanup(func() {
		srv.Close()
		st.Close()
	})
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
	if !strings.Contains(body, "Projects") {
		t.Fatalf("after register should land on projects, got:\n%s", body)
	}

	// The projects page is now reachable directly with the session cookie.
	resp, err = c.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get / authed: %v", err)
	}
	body = readBody(t, resp)
	if !strings.Contains(body, "grace") || !strings.Contains(body, "No sessions have been pushed yet") {
		t.Fatalf("projects page missing expected content, got:\n%s", body)
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

	// Search with no query renders the form without error.
	resp, err = c.Get(srv.URL + "/search")
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	body = readBody(t, resp)
	if !strings.Contains(body, "search message content") {
		t.Fatalf("search page missing form, got:\n%s", body)
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

func TestLoginPreservesNext(t *testing.T) {
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
	resp, err := c.Get(srv.URL + "/login?next=%2Fsearch")
	if err != nil {
		t.Fatalf("login page: %v", err)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `name="next" value="/search"`) {
		t.Fatalf("login page should carry next, got:\n%s", body)
	}

	// Stop following redirects so we can inspect the post-login Location.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err = c.PostForm(srv.URL+"/login", url.Values{
		"username": {"ada"}, "password": {"lovelace-1843"}, "next": {"/search"},
	})
	if err != nil {
		t.Fatalf("login post: %v", err)
	}
	if loc := resp.Header.Get("Location"); loc != "/search" {
		t.Fatalf("post-login redirect = %q, want /search", loc)
	}
	resp.Body.Close()
}

func TestPublicSessionFlow(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	c := newClient(t)

	// Seed an owner with one session carrying a searchable message.
	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari")
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
	if err := st.WriteProjection(ctx, sid, 0, store.Projection{
		ParserVersion: 1,
		MessageCount:  1,
		Messages: []store.ProjMessage{
			{Ordinal: 0, Role: "user", Content: "Fix the secret login bug"},
		},
	}); err != nil {
		t.Fatalf("write projection: %v", err)
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

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"":                  "/",
		"/projects/1":       "/projects/1",
		"//evil.example":    "/",
		"https://evil/x":    "/",
		"/search?q=a":       "/search?q=a",
		"javascript:alert1": "/",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
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
