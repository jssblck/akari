package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/reparse"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestProxyIdentityGates covers the branches that reject before ever touching the
// store, so they need no database: the mode being off, a blank identity header,
// and a shared-secret mismatch each report no asserted identity. A nil Store is
// safe here precisely because proxyIdentity never touches it; the store is reached
// only after it reports an asserted identity, on resolve's committed path.
func TestProxyIdentityGates(t *testing.T) {
	t.Parallel()

	newReq := func(headers map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		return r
	}

	cases := []struct {
		name    string
		cfg     config.Server
		headers map[string]string
	}{
		{
			name:    "disabled ignores the header",
			cfg:     config.Server{}, // ProxyAuthHeader empty: mode off
			headers: map[string]string{"X-Auth-Request-User": "grace"},
		},
		{
			name:    "absent identity header",
			cfg:     config.Server{ProxyAuthHeader: "X-Auth-Request-User"},
			headers: nil,
		},
		{
			name:    "blank identity header",
			cfg:     config.Server{ProxyAuthHeader: "X-Auth-Request-User"},
			headers: map[string]string{"X-Auth-Request-User": "   "},
		},
		{
			name: "missing shared secret",
			cfg: config.Server{
				ProxyAuthHeader:       "X-Auth-Request-User",
				ProxyAuthSecret:       "s3cret",
				ProxyAuthSecretHeader: "X-Akari-Proxy-Secret",
			},
			headers: map[string]string{"X-Auth-Request-User": "grace"},
		},
		{
			name: "wrong shared secret",
			cfg: config.Server{
				ProxyAuthHeader:       "X-Auth-Request-User",
				ProxyAuthSecret:       "s3cret",
				ProxyAuthSecretHeader: "X-Akari-Proxy-Secret",
			},
			headers: map[string]string{
				"X-Auth-Request-User":  "grace",
				"X-Akari-Proxy-Secret": "wrong",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &Server{Cfg: tc.cfg} // no Store: these paths must not reach it
			if _, ok := s.proxyIdentity(newReq(tc.headers)); ok {
				t.Fatalf("%s: proxyIdentity reported an asserted identity, want none", tc.name)
			}
		})
	}
}

// newProxyAuthServer brings up a full handler with proxy-header auth configured,
// backed by its own isolated database. It mirrors newTestServerWithReparse but
// takes the proxy-auth config the test wants to exercise.
func newProxyAuthServer(t *testing.T, cfg config.Server) (*httptest.Server, *store.Store) {
	t.Helper()
	st := storetest.NewStore(t)
	rp := reparse.New(context.Background(), st)
	srv := httptest.NewServer(New(st, cfg, rp).Routes())
	t.Cleanup(srv.Close)
	return srv, st
}

// TestProxyAuthProvisionsAndAuthenticates drives the trusted-proxy flow end to
// end: a request carrying the identity header provisions the account on first
// sight and reaches an authed-only page, a second request resolves the same
// account (no duplicate), the account is federated (no password, source "proxy",
// not admin), and it cannot log in through the password form.
func TestProxyAuthProvisionsAndAuthenticates(t *testing.T) {
	t.Parallel()
	const header = "X-Auth-Request-User"
	srv, st := newProxyAuthServer(t, config.Server{ProxyAuthHeader: header})

	// A read page that redirects an anonymous visitor to login. With the trusted
	// header set, the same request is authed and renders the account page.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/account", nil)
	req.Header.Set(header, "ada")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("GET /account with proxy header: %v", err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy-authed /account = %d, want 200; body:\n%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "ada") {
		t.Fatalf("account page missing the proxy-provisioned username, got:\n%s", body)
	}

	// The account was provisioned exactly once, as a federated user: no local
	// password, source "proxy", not admin.
	u, err := st.UserByUsername(context.Background(), "ada")
	if err != nil {
		t.Fatalf("proxy user not provisioned: %v", err)
	}
	if u.HasPassword() {
		t.Fatal("proxy-provisioned user should have no local password")
	}
	if u.AuthSource != "proxy" {
		t.Fatalf("auth_source = %q, want %q", u.AuthSource, "proxy")
	}
	if u.IsAdmin {
		t.Fatal("proxy-provisioned user must not be admin")
	}

	// A second request under the same identity resolves the same account rather
	// than minting a new one.
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/account", nil)
	req2.Header.Set(header, "ada")
	resp2, err := (&http.Client{}).Do(req2)
	if err != nil {
		t.Fatalf("second proxy-authed request: %v", err)
	}
	resp2.Body.Close()
	users, err := st.ListUsers(context.Background())
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("second request minted a duplicate account: %d users, want 1", len(users))
	}

	// The federated account has no password, so the login form refuses it (a 401,
	// with no hint the account exists) even though the account is real.
	status, got := postJSON(t, newClient(t), srv.URL+"/api/v1/auth/login", `{"username":"ada","password":"anything"}`)
	if status != http.StatusUnauthorized || got["error"] != "invalid credentials" {
		t.Fatalf("password login for federated account: status=%d body=%v, want 401 invalid credentials", status, got)
	}
}

// TestProxyAuthSharedSecret confirms the optional shared secret is enforced end to
// end: without the secret header the request is anonymous (bounced to login), and
// with the correct secret it is authed.
func TestProxyAuthSharedSecret(t *testing.T) {
	t.Parallel()
	const (
		header       = "X-Auth-Request-User"
		secretHeader = "X-Akari-Proxy-Secret"
		secret       = "shared-out-of-band"
	)
	srv, _ := newProxyAuthServer(t, config.Server{
		ProxyAuthHeader:       header,
		ProxyAuthSecret:       secret,
		ProxyAuthSecretHeader: secretHeader,
	})

	// Do not follow the redirect, so an unauthenticated request shows up as the
	// bounce to /login rather than the rendered login page (a 200).
	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// The identity header alone, without the secret, is not trusted: the request is
	// anonymous and an authed-only page bounces it to login.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/account", nil)
	req.Header.Set(header, "grace")
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatalf("GET /account without secret: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("proxy header without secret = %d, want a 303 bounce to login", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("bounce location = %q, want /login...", loc)
	}

	// With the correct secret the same request is authed.
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/account", nil)
	req2.Header.Set(header, "grace")
	req2.Header.Set(secretHeader, secret)
	resp2, err := noFollow.Do(req2)
	if err != nil {
		t.Fatalf("GET /account with secret: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("proxy header with correct secret = %d, want 200", resp2.StatusCode)
	}
}

// TestProxyAuthFailsClosedOnStoreError confirms that once a trusted proxy identity
// is asserted, a failure to resolve it fails closed rather than falling through to
// the cookie. It closes the store so provisioning errors, sends a request that
// carries both the identity header and a session cookie, and asserts the request
// is bounced to login: had resolve fallen through on the error, the cookie branch
// would have been consulted. (That fallthrough is prevented structurally by
// resolve returning inside its committed proxy block; this guards the error path
// end to end.)
func TestProxyAuthFailsClosedOnStoreError(t *testing.T) {
	t.Parallel()
	const header = "X-Auth-Request-User"
	srv, st := newProxyAuthServer(t, config.Server{ProxyAuthHeader: header})

	// Break the store so UpsertProxyUser cannot succeed.
	st.Close()

	noFollow := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/account", nil)
	req.Header.Set(header, "ada")
	req.AddCookie(&http.Cookie{Name: cookieName, Value: "some-stale-cookie"})
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatalf("GET /account with broken store: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("asserted proxy identity with a failing store = %d, want a 303 bounce to login (fail closed)", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Fatalf("bounce location = %q, want /login...", loc)
	}
}

// TestProxyAuthDoesNotShadowBearer confirms an explicit Bearer credential wins
// over the proxy header, so a coding agent hitting an API with its own token is
// that token's principal, not the browser user the proxy names. The read-scope
// token here is rejected by the ingest gate; the point is that the proxy header
// did not silently authenticate the request as a full-scope user instead.
func TestProxyAuthDoesNotShadowBearer(t *testing.T) {
	t.Parallel()
	const header = "X-Auth-Request-User"
	srv, st := newProxyAuthServer(t, config.Server{ProxyAuthHeader: header})

	// Seed an account with a read-scope token.
	ctx := context.Background()
	u, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	token := "read-only-token"
	if _, err := st.CreateAPIToken(ctx, u.ID, "reader", "read", auth.HashToken(token)); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	// The ingest endpoint accepts ingest/full, not read. A request carrying both a
	// read Bearer token and the proxy identity header must be judged as the read
	// token (403 forbidden), not as the full-scope proxy user (which would pass the
	// gate). This proves Bearer is resolved ahead of the proxy header.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/v1/ingest/session", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(header, "ada")
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("ingest with bearer+proxy: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("ingest resolved as %d; want 403 (read token), proving Bearer beat the proxy header", resp.StatusCode)
	}
}
