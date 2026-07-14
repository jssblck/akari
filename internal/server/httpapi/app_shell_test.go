package httpapi

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAppShellLoginRedirectPreservesQuery(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	response := mustGet(t, client, srv.URL+"/sessions?sort=cost&dir=asc")
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET private shell = %d, want %d", response.StatusCode, http.StatusSeeOther)
	}
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse login redirect: %v", err)
	}
	if got, want := location.Path, "/login"; got != want {
		t.Fatalf("redirect path = %q, want %q", got, want)
	}
	if got, want := location.Query().Get("next"), "/sessions?sort=cost&dir=asc"; got != want {
		t.Fatalf("next = %q, want %q", got, want)
	}
}

// A dead public link must land on the React app's styled error state, not a
// bare text page, so the shell handlers serve the entry document with the
// error status and let the client render "Not found" with a way back home.
func TestPublicShellLookupFailureServesAppShell(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)

	for _, path := range []string{"/p/999999", "/u/nobody", "/s/no-such-public-id"} {
		resp := mustGet(t, http.DefaultClient, srv.URL+path)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("read %s body: %v", path, err)
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, http.StatusNotFound)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("GET %s Content-Type = %q, want text/html shell", path, ct)
		}
		if !strings.Contains(string(body), `id="root"`) {
			t.Errorf("GET %s body is not the app shell (no root mount point)", path)
		}
	}
}
