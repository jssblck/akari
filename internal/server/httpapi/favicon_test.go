package httpapi

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// The handler is static and store-independent, so it runs without a database:
// this pins the exact response an unfurler or browser sees (the .ico bytes, the
// image/x-icon type, a matching Content-Length, and the day-long cache) so a
// regression that drops cacheability, changes the type, or miscounts the length
// fails here rather than slipping through the broad checks in the routed test.
func TestFaviconICOHandler(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/favicon.ico", nil)
	(&Server{}).handleFaviconICO(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Content-Type"), "image/x-icon"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if got, want := resp.Header.Get("Cache-Control"), fmt.Sprintf("public, max-age=%d", landingOGCacheMaxAge); got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
	if got, want := resp.Header.Get("Content-Length"), strconv.Itoa(len(faviconICO)); got != want {
		t.Errorf("Content-Length = %q, want %q", got, want)
	}
	if body := rec.Body.Bytes(); !bytes.Equal(body, faviconICO) {
		t.Errorf("body is %d bytes, want the %d embedded .ico bytes", len(body), len(faviconICO))
	}
}

// The routed test proves the wiring the handler test cannot: that /favicon.ico is
// reachable at the root through the real mux and served to an anonymous visitor
// (not 404, not a redirect to login). It gates on AKARI_TEST_DATABASE_URL like
// the rest of the server tests, so the handler test above carries the assertions
// that must run on every same-package go test.
func TestFaviconICOIsPublic(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)

	c := newClient(t)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	resp := mustGet(t, c, srv.URL+"/favicon.ico")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /favicon.ico = %d, want 200 (served at the root, no auth)", resp.StatusCode)
	}
}
