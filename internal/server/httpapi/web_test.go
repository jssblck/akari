package httpapi

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/migrations"
)

// newTestServer brings up a full Routes() handler backed by a freshly migrated
// test database. It is skipped unless AKARI_TEST_DATABASE_URL is set.
func newTestServer(t *testing.T) *httptest.Server {
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
	return srv
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
	srv := newTestServer(t)
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
	srv := newTestServer(t)
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
