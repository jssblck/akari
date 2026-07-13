package httpapi

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

func mustHash(t *testing.T, password string) string {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	return hash
}

func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	server, st, _ := newTestServerWithReparse(t)
	return server, st
}

func newTestServerWithReparse(t *testing.T) (*httptest.Server, *store.Store, *parse.Worker) {
	t.Helper()
	st := storetest.NewStore(t)
	worker := parse.NewWorker(st, 1, 0)
	server := httptest.NewServer(New(st, config.Server{}, worker).Routes())
	t.Cleanup(server.Close)
	return server, st, worker
}

func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("create cookie jar: %v", err)
	}
	return &http.Client{Jar: jar, Transport: browserRoundTripper{base: http.DefaultTransport}}
}

func registerAdmin(t *testing.T, serverURL string) *http.Client {
	t.Helper()
	client := newClient(t)
	status, body := postJSON(t, client, serverURL+"/api/v1/auth/register", `{"username":"grace","password":"hopper-1906"}`)
	if status != http.StatusCreated {
		t.Fatalf("register admin: status=%d body=%v", status, body)
	}
	return client
}

type browserRoundTripper struct{ base http.RoundTripper }

func (b browserRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if isSafeMethod(r.Method) {
		return b.base.RoundTrip(r)
	}
	request := r.Clone(r.Context())
	request.Header.Set("Origin", request.URL.Scheme+"://"+request.URL.Host)
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	return b.base.RoundTrip(request)
}

type stubReducer struct{ delta store.ProjectionDelta }

func (r stubReducer) Feed([]byte, int64) error      { return nil }
func (r stubReducer) Finish() store.ProjectionDelta { return r.delta }

func rebuildWith(t *testing.T, st *store.Store, sessionID int64, delta store.ProjectionDelta) {
	t.Helper()
	if err := st.RebuildSession(context.Background(), sessionID, parse.Epoch, stubReducer{delta: delta}); err != nil {
		t.Fatalf("rebuild session %d: %v", sessionID, err)
	}
}

func stampSessionCurrent(t *testing.T, st *store.Store, sessionID int64) {
	t.Helper()
	if _, err := st.Pool.Exec(context.Background(),
		"UPDATE session_raw SET parser_epoch = $2 WHERE session_id = $1", sessionID, parse.Epoch); err != nil {
		t.Fatalf("stamp session %d current: %v", sessionID, err)
	}
}

func mustGet(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	response, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return response
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return string(body)
}
