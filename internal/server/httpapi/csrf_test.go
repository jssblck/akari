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

// cookieValue reads a named cookie's current value out of a client's jar for
// the given URL, or "" if the jar holds none. Tests use it to observe the
// CSRF cookie rotating underneath a browser session without parsing raw
// Set-Cookie headers by hand.
func cookieValue(t *testing.T, c *http.Client, rawURL, name string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == name {
			return ck.Value
		}
	}
	return ""
}

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

// TestCSRFDerivedOriginThroughPortForward covers the dev loop where the
// browser reaches the server through a TCP port forward (eph dev behind the
// Claude preview gate): the forwarded request carries the gate port in both
// Origin and Host. With no public URL configured the trust boundary is the
// request's own host, so that login succeeds, while the server's internal
// auto-assigned port stays a foreign origin even though both are loopback.
func TestCSRFDerivedOriginThroughPortForward(t *testing.T) {
	t.Parallel()
	s := &Server{}

	request := func(origin string) int {
		req := httptest.NewRequest(http.MethodPost, "http://localhost:8080/api/v1/auth/login", nil)
		req.Host = "localhost:8080"
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		s.withCSRF(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		})).ServeHTTP(rec, req)
		return rec.Code
	}

	if got := request("http://localhost:8080"); got != http.StatusNoContent {
		t.Fatalf("forwarded-port login = %d, want %d", got, http.StatusNoContent)
	}
	if got := request("http://localhost:60663"); got != http.StatusForbidden {
		t.Fatalf("cross-port loopback origin = %d, want %d", got, http.StatusForbidden)
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

// TestCSRFCookieRotatesOnLogin confirms a token minted before sign-in stops
// working immediately after: an attacker who planted or observed that token
// ahead of the victim logging in cannot go on using it once the privilege
// change lands.
func TestCSRFCookieRotatesOnLogin(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	browser := newClient(t)

	mustGet(t, browser, srv.URL+"/login").Body.Close()
	preLoginToken := cookieValue(t, browser, srv.URL, csrfCookieName)
	if preLoginToken == "" {
		t.Fatal("no CSRF cookie minted before login")
	}

	status, _ := postJSON(t, browser, srv.URL+"/api/v1/auth/register", `{"username":"grace","password":"hopper-1906"}`)
	if status != http.StatusCreated {
		t.Fatalf("register status = %d, want %d", status, http.StatusCreated)
	}

	postLoginToken := cookieValue(t, browser, srv.URL, csrfCookieName)
	if postLoginToken == "" || postLoginToken == preLoginToken {
		t.Fatalf("CSRF cookie did not rotate on login: pre=%q post=%q", preLoginToken, postLoginToken)
	}

	// Present the pre-login token via cookie and header with no Origin or
	// Sec-Fetch-Site, so validation rests entirely on the double-submit
	// match, the path a non-browser client (and a forging attacker) takes.
	raw := &http.Client{Jar: browser.Jar}
	stale, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/tokens", strings.NewReader(`{"name":"stolen"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	stale.Header.Set("Content-Type", "application/json")
	stale.Header.Set(csrfHeaderName, preLoginToken)
	staleResp, err := raw.Do(stale)
	if err != nil {
		t.Fatalf("stale-token mutation: %v", err)
	}
	staleResp.Body.Close()
	if staleResp.StatusCode != http.StatusForbidden {
		t.Fatalf("mutation with pre-login CSRF token = %d, want %d", staleResp.StatusCode, http.StatusForbidden)
	}

	// Sanity: the current token still authorizes the same request, so the
	// rejection above is about staleness, not a broken double-submit check.
	fresh, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/tokens", strings.NewReader(`{"name":"legitimate"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	fresh.Header.Set("Content-Type", "application/json")
	fresh.Header.Set(csrfHeaderName, postLoginToken)
	freshResp, err := raw.Do(fresh)
	if err != nil {
		t.Fatalf("current-token mutation: %v", err)
	}
	freshResp.Body.Close()
	if freshResp.StatusCode != http.StatusCreated {
		t.Fatalf("mutation with current CSRF token = %d, want %d", freshResp.StatusCode, http.StatusCreated)
	}
}

// TestCSRFCookieRotatesOnLogout confirms logout invalidates the session's
// CSRF token the same way login does: a token captured before sign-out must
// not go on authorizing requests once the session is gone.
func TestCSRFCookieRotatesOnLogout(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	browser := newClient(t)

	status, _ := postJSON(t, browser, srv.URL+"/api/v1/auth/register", `{"username":"grace","password":"hopper-1906"}`)
	if status != http.StatusCreated {
		t.Fatalf("register status = %d, want %d", status, http.StatusCreated)
	}
	preLogoutToken := cookieValue(t, browser, srv.URL, csrfCookieName)
	if preLogoutToken == "" {
		t.Fatal("no CSRF cookie after registration")
	}

	status, _ = postJSON(t, browser, srv.URL+"/api/v1/auth/logout", `{}`)
	if status != http.StatusOK {
		t.Fatalf("logout status = %d, want %d", status, http.StatusOK)
	}

	postLogoutToken := cookieValue(t, browser, srv.URL, csrfCookieName)
	if postLogoutToken == "" || postLogoutToken == preLogoutToken {
		t.Fatalf("CSRF cookie did not rotate on logout: pre=%q post=%q", preLogoutToken, postLogoutToken)
	}

	// The pre-logout token, presented via cookie and header with no Origin or
	// Sec-Fetch-Site, must be rejected by the CSRF gate itself (403), not
	// merely by the now-absent session (401).
	raw := &http.Client{Jar: browser.Jar}
	stale, err := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/tokens", strings.NewReader(`{"name":"stolen"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	stale.Header.Set("Content-Type", "application/json")
	stale.Header.Set(csrfHeaderName, preLogoutToken)
	staleResp, err := raw.Do(stale)
	if err != nil {
		t.Fatalf("stale-token mutation: %v", err)
	}
	staleResp.Body.Close()
	if staleResp.StatusCode != http.StatusForbidden {
		t.Fatalf("mutation with pre-logout CSRF token = %d, want %d", staleResp.StatusCode, http.StatusForbidden)
	}
}
