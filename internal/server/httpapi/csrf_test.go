package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/web"
)

func TestCSRFOriginAndFetchMetadataPolicy(t *testing.T) {
	t.Parallel()
	s := &Server{Cfg: config.Server{PublicURL: "https://akari.example"}}
	token, err := auth.NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}

	tests := []struct {
		name       string
		origin     []string
		fetchSite  []string
		withToken  bool
		wantStatus int
	}{
		{name: "matching origin", origin: []string{"https://akari.example"}, wantStatus: http.StatusNoContent},
		{name: "matching origin and fetch metadata", origin: []string{"https://akari.example"}, fetchSite: []string{"same-origin"}, wantStatus: http.StatusNoContent},
		{name: "fetch metadata only", fetchSite: []string{"same-origin"}, wantStatus: http.StatusNoContent},
		{name: "default HTTPS port is equivalent", origin: []string{"https://AKARI.example:443"}, wantStatus: http.StatusNoContent},
		{name: "cross origin", origin: []string{"https://attacker.example"}, wantStatus: http.StatusForbidden},
		{name: "same site sibling", origin: []string{"https://notes.example"}, fetchSite: []string{"same-site"}, wantStatus: http.StatusForbidden},
		{name: "conflicting fetch metadata", origin: []string{"https://akari.example"}, fetchSite: []string{"cross-site"}, wantStatus: http.StatusForbidden},
		{name: "opaque origin", origin: []string{"null"}, withToken: true, wantStatus: http.StatusForbidden},
		{name: "origin with path", origin: []string{"https://akari.example/path"}, withToken: true, wantStatus: http.StatusForbidden},
		{name: "multiple origins", origin: []string{"https://akari.example", "https://attacker.example"}, withToken: true, wantStatus: http.StatusForbidden},
		{name: "multiple fetch values", fetchSite: []string{"same-origin", "same-site"}, withToken: true, wantStatus: http.StatusForbidden},
		{name: "missing signals", wantStatus: http.StatusForbidden},
		{name: "missing signals with token", withToken: true, wantStatus: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://internal.local/account/tokens", nil)
			for _, value := range tt.origin {
				req.Header.Add("Origin", value)
			}
			for _, value := range tt.fetchSite {
				req.Header.Add("Sec-Fetch-Site", value)
			}
			if tt.withToken {
				req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: token})
				req.Header.Set(csrfHeaderName, token)
			}
			rec := httptest.NewRecorder()
			s.withCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestCSRFTokenFallbackFromRenderedForm(t *testing.T) {
	t.Parallel()
	s := &Server{Cfg: config.Server{}}
	handler := s.withCSRF(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`<form><input name="_csrf" value="` + webToken(r) + `"></form>`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	get := httptest.NewRequest(http.MethodGet, "http://akari.example/login", nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, get)
	cookies := getRec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != csrfCookieName {
		t.Fatalf("GET /login cookies = %+v, want %s", cookies, csrfCookieName)
	}
	token := cookies[0].Value
	if !strings.Contains(getRec.Body.String(), `value="`+token+`"`) {
		t.Fatalf("rendered form did not receive CSRF token: %s", getRec.Body.String())
	}

	form := url.Values{csrfFormName: {token}}
	post := httptest.NewRequest(http.MethodPost, "http://akari.example/login", strings.NewReader(form.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.AddCookie(cookies[0])
	postRec := httptest.NewRecorder()
	handler.ServeHTTP(postRec, post)
	if postRec.Code != http.StatusNoContent {
		t.Fatalf("token fallback status = %d, want %d; body=%s", postRec.Code, http.StatusNoContent, postRec.Body.String())
	}
}

func webToken(r *http.Request) string {
	return web.CSRFToken(r.Context())
}

func TestCSRFTrustedProxyUsesConfiguredPublicOrigin(t *testing.T) {
	t.Parallel()
	s := &Server{Cfg: config.Server{
		PublicURL:       "https://akari.example",
		ProxyAuthHeader: "X-Auth-Request-User",
	}}

	request := func(origin string) int {
		req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/account/reparse", nil)
		req.Host = "127.0.0.1:8080"
		req.Header.Set("X-Forwarded-Proto", "http")
		req.Header.Set("X-Auth-Request-User", "grace")
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		s.withCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(rec, req)
		return rec.Code
	}

	if got := request("https://akari.example"); got != http.StatusNoContent {
		t.Fatalf("public origin through proxy = %d, want %d", got, http.StatusNoContent)
	}
	if got := request("http://127.0.0.1:8080"); got != http.StatusForbidden {
		t.Fatalf("internal upstream origin = %d, want %d", got, http.StatusForbidden)
	}
}

func TestCSRFDynamicOriginHonorsForwardedScheme(t *testing.T) {
	t.Parallel()
	s := &Server{}
	req := httptest.NewRequest(http.MethodPost, "http://internal.local/login", nil)
	req.Host = "akari.example"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("Origin", "https://akari.example")
	rec := httptest.NewRecorder()
	s.withCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("forwarded origin status = %d, want %d; body=%s", rec.Code, http.StatusNoContent, rec.Body.String())
	}
}

func TestCSRFExemptsBearerAndNonCookieProtocolEndpoints(t *testing.T) {
	t.Parallel()
	s := &Server{Cfg: config.Server{PublicURL: "https://akari.example"}}
	tests := []struct {
		name   string
		path   string
		bearer bool
	}{
		{name: "bearer ingest", path: "/api/v1/ingest/session", bearer: true},
		{name: "OAuth registration", path: oauthRegisterPath},
		{name: "OAuth token", path: oauthTokenPath},
		{name: "MCP", path: mcpPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "https://akari.example"+tt.path, nil)
			if tt.bearer {
				req.Header.Set("Authorization", "Bearer token")
			}
			rec := httptest.NewRecorder()
			s.withCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			})).ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusNoContent)
			}
		})
	}
}

func TestCSRFRoutesRejectLoginAndCookieMutation(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	browser := newClient(t)

	status, _ := postJSON(t, browser, srv.URL+"/api/v1/auth/register", `{"username":"grace","password":"hopper-1906"}`)
	if status != http.StatusCreated {
		t.Fatalf("register status = %d, want %d", status, http.StatusCreated)
	}
	status, _ = postJSON(t, browser, srv.URL+"/api/v1/auth/logout", `{}`)
	if status != http.StatusOK {
		t.Fatalf("logout status = %d, want %d", status, http.StatusOK)
	}

	raw := &http.Client{Jar: browser.Jar}
	loginBody := []byte(`{"username":"grace","password":"hopper-1906"}`)
	loginReq, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/auth/login", bytes.NewReader(loginBody))
	if err != nil {
		t.Fatalf("new login request: %v", err)
	}
	loginReq.Header.Set("Content-Type", "application/json")
	loginReq.Header.Set("Origin", "https://attacker.example")
	resp, err := raw.Do(loginReq)
	if err != nil {
		t.Fatalf("cross-origin login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin login = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	status, _ = postJSON(t, browser, srv.URL+"/api/v1/auth/login", string(loginBody))
	if status != http.StatusOK {
		t.Fatalf("same-origin login status = %d, want %d", status, http.StatusOK)
	}

	mutationReq, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/tokens", strings.NewReader(`{"name":"stolen"}`))
	if err != nil {
		t.Fatalf("new mutation request: %v", err)
	}
	mutationReq.Header.Set("Content-Type", "application/json")
	mutationReq.Header.Set("Origin", "http://sibling.example")
	mutationReq.Header.Set("Sec-Fetch-Site", "same-site")
	resp, err = raw.Do(mutationReq)
	if err != nil {
		t.Fatalf("same-site sibling mutation: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("same-site sibling mutation = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	resp = mustGet(t, browser, srv.URL+"/api/v1/tokens")
	defer resp.Body.Close()
	var listed struct {
		Tokens []any `json:"tokens"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatalf("decode tokens: %v", err)
	}
	if len(listed.Tokens) != 0 {
		t.Fatalf("cross-site mutation created tokens: %+v", listed.Tokens)
	}
}
