package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// Browsers hit /favicon.ico at the root without being told to, so the route must
// serve the embedded icon (not 404, and not redirect to login) for an anonymous
// visitor. It gates on AKARI_TEST_DATABASE_URL like the rest of the server tests.
func TestFaviconICOIsPublic(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)

	c := newClient(t)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp := mustGet(t, c, srv.URL+"/favicon.ico")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /favicon.ico = %d, want 200 (served at the root, no auth)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/") {
		t.Errorf("GET /favicon.ico Content-Type = %q, want an image type", ct)
	}
	if body := readBody(t, resp); len(body) == 0 {
		t.Error("GET /favicon.ico returned an empty body")
	}
}
