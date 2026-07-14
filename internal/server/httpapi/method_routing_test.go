package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

func TestRetiredFormRoutesStayRemoved(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)

	for _, test := range []struct {
		path   string
		status int
	}{
		{path: "/login", status: http.StatusMethodNotAllowed},
		{path: "/register", status: http.StatusMethodNotAllowed},
		{path: "/logout", status: http.StatusNotFound},
	} {
		t.Run(test.path, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodPost, srv.URL+test.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatalf("POST %s: %v", test.path, err)
			}
			response.Body.Close()
			if response.StatusCode != test.status {
				t.Fatalf("POST %s = %d, want %d", test.path, response.StatusCode, test.status)
			}
			if allow := response.Header.Get("Allow"); strings.Contains(allow, http.MethodPost) {
				t.Fatalf("POST %s remains allowed: %q", test.path, allow)
			}
		})
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
