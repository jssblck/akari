package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestWrongMethodPreservesServeMuxResponse(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/login", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /login = %d, want %d", resp.StatusCode, http.StatusMethodNotAllowed)
	}
	allow := resp.Header.Get("Allow")
	if !strings.Contains(allow, http.MethodGet) || !strings.Contains(allow, http.MethodPost) {
		t.Fatalf("Allow = %q, want GET and POST", allow)
	}
}

func TestUnknownPathUsesStyledNotFound(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/missing-page")
	if err != nil {
		t.Fatalf("GET missing page: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET missing page = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(body, "That page does not exist") {
		t.Fatalf("missing page did not use styled HTML response: %q", body)
	}
}
