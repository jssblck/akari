package httpapi

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

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
