package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/auth"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// registerGrace creates the first account (admin) through the browser flow, so the
// cookie jar on c carries a live session afterward.
func registerGrace(t *testing.T, srv string, c *http.Client) {
	t.Helper()
	resp, err := c.PostForm(srv+"/register", url.Values{"username": {"grace"}, "password": {"hopper-1906"}})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	resp.Body.Close()
}

// bearerRT injects a bearer token on every request, so an MCP client transport
// authenticates as the token's owner.
type bearerRT struct {
	base  http.RoundTripper
	token string
}

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// hostRewriteRT injects a bearer token and forces the outgoing Host header, so a
// test can dial the loopback httptest server while presenting the public Host a
// reverse proxy would forward. This reproduces the deployment shape (Caddy dials
// akari over loopback, forwards Host: akari.jessica.black) that trips the go-sdk's
// loopback DNS-rebinding guard.
type hostRewriteRT struct {
	base  http.RoundTripper
	token string
	host  string
}

func (h hostRewriteRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Host = h.host
	r.Header.Set("Authorization", "Bearer "+h.token)
	return h.base.RoundTrip(r)
}

// mcpSession dials the MCP endpoint with the given bearer token and returns an
// initialized client session.
func mcpSession(t *testing.T, srvURL, token string) *mcpsdk.ClientSession {
	t.Helper()
	transport := &mcpsdk.StreamableClientTransport{
		Endpoint:             srvURL + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerRT{base: http.DefaultTransport, token: token}},
		DisableStandaloneSSE: true,
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	sess, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect mcp: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// callToolJSON calls a tool and unmarshals its structured result into out.
func callToolJSON(t *testing.T, sess *mcpsdk.ClientSession, name string, args any, out any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("call %s returned tool error: %+v", name, res.Content)
	}
	if out != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatalf("call %s: marshal structuredContent: %v", name, err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("call %s: unmarshal structuredContent: %v", name, err)
		}
	}
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// runOAuthFlow drives the full browser consent dance and returns the issued token
// response. It registers a client, authorizes with PKCE as the signed-in user,
// approves consent, and exchanges the code.
func runOAuthFlow(t *testing.T, srvURL string, c *http.Client) map[string]any {
	t.Helper()

	// Dynamic client registration.
	redirectURI := "http://127.0.0.1:9999/callback"
	regBody, _ := json.Marshal(map[string]any{"client_name": "Grace's agent", "redirect_uris": []string{redirectURI}})
	resp, err := c.Post(srvURL+"/oauth/register", "application/json", strings.NewReader(string(regBody)))
	if err != nil {
		t.Fatalf("register client: %v", err)
	}
	var reg map[string]any
	decodeBody(t, resp, &reg)
	clientID, _ := reg["client_id"].(string)
	if clientID == "" {
		t.Fatalf("registration returned no client_id: %+v", reg)
	}

	// PKCE pair.
	verifier := "verifier-grace-hopper-1906-anna-winlock-ada-lovelace"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	// Authorize: signed-in browser lands on the consent page.
	authzURL := srvURL + "/oauth/authorize?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"state-123"},
		"scope":                 {"read"},
		"resource":              {srvURL + "/mcp"},
	}.Encode()
	resp, err = c.Get(authzURL)
	if err != nil {
		t.Fatalf("authorize GET: %v", err)
	}
	page := readBody(t, resp)
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("consent page missing csrf field:\n%s", page)
	}
	csrf := m[1]

	// Approve consent; capture the redirect to the client without following it.
	noFollow := &http.Client{Jar: c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err = noFollow.PostForm(srvURL+"/oauth/authorize", url.Values{
		"client_id":      {clientID},
		"redirect_uri":   {redirectURI},
		"state":          {"state-123"},
		"code_challenge": {challenge},
		"resource":       {srvURL + "/mcp"},
		"csrf":           {csrf},
		"decision":       {"approve"},
	})
	if err != nil {
		t.Fatalf("authorize POST: %v", err)
	}
	resp.Body.Close()
	loc, err := resp.Location()
	if err != nil {
		t.Fatalf("decision redirect: %v", err)
	}
	if got := loc.Query().Get("state"); got != "state-123" {
		t.Fatalf("redirect state = %q, want state-123", got)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("redirect carried no code: %s", loc.String())
	}

	// A failed binding check must not burn the code. Clients can recover from a
	// locally misconfigured verifier by retrying the still-valid exchange.
	resp, err = c.PostForm(srvURL+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {"wrong-verifier"},
	})
	if err != nil {
		t.Fatalf("invalid token exchange: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Fatalf("invalid token exchange status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	var oauthErr map[string]any
	decodeBody(t, resp, &oauthErr)
	if oauthErr["error"] != "invalid_grant" {
		t.Fatalf("invalid token exchange error = %v, want invalid_grant", oauthErr["error"])
	}

	// Exchange the code for tokens.
	resp, err = c.PostForm(srvURL+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	})
	if err != nil {
		t.Fatalf("token exchange: %v", err)
	}
	var tok map[string]any
	decodeBody(t, resp, &tok)
	if tok["access_token"] == nil || tok["refresh_token"] == nil {
		t.Fatalf("token response missing tokens: %+v", tok)
	}
	tok["client_id"] = clientID
	return tok
}

func decodeBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode body: %v", err)
	}
}

func TestOAuthDiscoveryMetadata(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)

	var prm map[string]any
	resp, err := http.Get(srv.URL + "/.well-known/oauth-protected-resource")
	if err != nil {
		t.Fatalf("get protected-resource metadata: %v", err)
	}
	decodeBody(t, resp, &prm)
	if prm["resource"] != srv.URL+"/mcp" {
		t.Fatalf("resource = %v, want %s/mcp", prm["resource"], srv.URL)
	}

	var asm map[string]any
	resp, err = http.Get(srv.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatalf("get auth-server metadata: %v", err)
	}
	decodeBody(t, resp, &asm)
	if asm["token_endpoint"] != srv.URL+"/oauth/token" || asm["authorization_endpoint"] != srv.URL+"/oauth/authorize" {
		t.Fatalf("auth-server metadata endpoints wrong: %+v", asm)
	}
	methods, _ := asm["code_challenge_methods_supported"].([]any)
	if len(methods) != 1 || methods[0] != "S256" {
		t.Fatalf("expected S256-only PKCE, got %+v", asm["code_challenge_methods_supported"])
	}
}

func TestMCPEndpointRequiresAuth(t *testing.T) {
	t.Parallel()
	srv, _ := newTestServer(t)
	// A POST with no bearer is unauthorized, and the challenge points at the
	// protected-resource metadata so a client can discover how to authenticate.
	resp, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		t.Fatalf("post /mcp: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token /mcp status = %d, want 401", resp.StatusCode)
	}
	if h := resp.Header.Get("WWW-Authenticate"); !strings.Contains(h, "resource_metadata") {
		t.Fatalf("WWW-Authenticate missing resource_metadata: %q", h)
	}
}

func TestOAuthFlowEndToEndAndMCP(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	c := newClient(t)
	registerGrace(t, srv.URL, c)

	tok := runOAuthFlow(t, srv.URL, c)
	access := tok["access_token"].(string)

	// The minted access token authenticates the MCP session as grace.
	sess := mcpSession(t, srv.URL, access)
	var who struct {
		Username string `json:"username"`
		IsAdmin  bool   `json:"is_admin"`
	}
	callToolJSON(t, sess, "whoami", map[string]any{}, &who)
	if who.Username != "grace" || !who.IsAdmin {
		t.Fatalf("whoami = %+v, want grace/admin", who)
	}

	// A read tool works against the empty instance.
	var projects struct {
		Projects []any `json:"projects"`
	}
	callToolJSON(t, sess, "list_projects", map[string]any{}, &projects)
	if projects.Projects == nil {
		t.Fatalf("list_projects returned nil projects slice")
	}

	// The refresh grant rotates a fresh, working access token.
	resp, err := c.PostForm(srv.URL+"/oauth/token", url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {tok["refresh_token"].(string)},
		"client_id":     {tok["client_id"].(string)},
	})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	var refreshed map[string]any
	decodeBody(t, resp, &refreshed)
	newAccess, _ := refreshed["access_token"].(string)
	if newAccess == "" || newAccess == access {
		t.Fatalf("refresh did not rotate the access token: %+v", refreshed)
	}
	sess2 := mcpSession(t, srv.URL, newAccess)
	callToolJSON(t, sess2, "whoami", map[string]any{}, &who)
	if who.Username != "grace" {
		t.Fatalf("post-refresh whoami = %+v", who)
	}

	// Disconnecting the app revokes its tokens: the rotated access token stops
	// authenticating the MCP endpoint.
	u, err := st.UserByUsername(context.Background(), "grace")
	if err != nil {
		t.Fatalf("lookup grace: %v", err)
	}
	if err := st.RevokeOAuthGrant(context.Background(), u.ID, tok["client_id"].(string)); err != nil {
		t.Fatalf("revoke grant: %v", err)
	}
	transport := &mcpsdk.StreamableClientTransport{
		Endpoint:             srv.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerRT{base: http.DefaultTransport, token: newAccess}},
		DisableStandaloneSSE: true,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, transport, nil); err == nil {
		t.Fatalf("expected connect to fail after the grant was revoked")
	}
}

// TestMCPAllowsProxiedHost pins the fix for the remote-deployment 403. The go-sdk
// auto-enables a DNS-rebinding guard when the accepted connection is loopback and
// then rejects any non-loopback Host with 403 Forbidden. A production akari sits
// behind a reverse proxy that dials it over loopback while forwarding the public
// Host, so without DisableLocalhostProtection every authenticated /mcp request
// 403s: OAuth completes, then the first real request is rejected, which the client
// reports as its new credentials being refused on reconnect. httptest listens on
// loopback, so presenting a non-loopback Host here recreates that shape exactly.
func TestMCPAllowsProxiedHost(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register grace: %v", err)
	}
	secret, err := auth.NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	if _, err := st.CreateAPIToken(ctx, u.ID, "read tok", "read", auth.HashToken(secret)); err != nil {
		t.Fatalf("create read token: %v", err)
	}

	// Dial the loopback server but forward a public Host, the way Caddy would. This
	// connects (rather than 403ing) only because the handler disables the loopback
	// guard; the whoami round-trip proves the request reached the tools.
	transport := &mcpsdk.StreamableClientTransport{
		Endpoint:             srv.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: hostRewriteRT{base: http.DefaultTransport, token: secret, host: "akari.jessica.black"}},
		DisableStandaloneSSE: true,
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sess, err := client.Connect(cctx, transport, nil)
	if err != nil {
		t.Fatalf("connect through a forwarded non-loopback Host: %v", err)
	}
	defer sess.Close()

	var who struct {
		Username string `json:"username"`
	}
	callToolJSON(t, sess, "whoami", map[string]any{}, &who)
	if who.Username != "grace" {
		t.Fatalf("whoami = %+v, want grace", who)
	}
}

func TestMCPAcceptsReadTokenRejectsIngest(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "ada", mustHash(t, "lovelace-1843"), "")
	if err != nil {
		t.Fatalf("register ada: %v", err)
	}

	// A read-scope API token (created from the account page in practice) reaches the
	// MCP endpoint directly.
	readSecret, _ := auth.NewToken()
	if _, err := st.CreateAPIToken(ctx, u.ID, "read tok", "read", auth.HashToken(readSecret)); err != nil {
		t.Fatalf("create read token: %v", err)
	}
	sess := mcpSession(t, srv.URL, readSecret)
	var who struct {
		Username string `json:"username"`
	}
	callToolJSON(t, sess, "whoami", map[string]any{}, &who)
	if who.Username != "ada" {
		t.Fatalf("read-token whoami = %+v, want ada", who)
	}

	// An ingest-scope token must not reach the read surface.
	ingestSecret, _ := auth.NewToken()
	if _, err := st.CreateAPIToken(ctx, u.ID, "ingest tok", "ingest", auth.HashToken(ingestSecret)); err != nil {
		t.Fatalf("create ingest token: %v", err)
	}
	transport := &mcpsdk.StreamableClientTransport{
		Endpoint:             srv.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerRT{base: http.DefaultTransport, token: ingestSecret}},
		DisableStandaloneSSE: true,
	}
	ctx2, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx2, transport, nil); err == nil {
		t.Fatalf("expected ingest token to be rejected at /mcp")
	}
}
