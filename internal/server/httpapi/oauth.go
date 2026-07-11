package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/web"
)

// The OAuth 2.1 authorization server backing the remote MCP endpoint. akari is its
// own identity provider: the authorize endpoint recognizes the browser session
// cookie a user already holds, so connecting a coding agent is a single consent
// click, never a password typed into the agent. The flow is the one MCP clients
// implement out of the box: discover the metadata, register dynamically, redirect
// through authorize with PKCE, and exchange the code for a bearer token here.
//
// Scope is read-only by construction (see scopeRead): a token minted through this
// flow can read everything a logged-in user sees and change nothing.

const (
	mcpPath            = "/mcp"
	resourceMetaPath   = "/.well-known/oauth-protected-resource"
	authServerMetaPath = "/.well-known/oauth-authorization-server"
	oauthAuthorizePath = "/oauth/authorize"
	oauthTokenPath     = "/oauth/token"
	oauthRegisterPath  = "/oauth/register"

	// oauthCodeTTL bounds how long an authorization code is redeemable: long enough
	// for the client to complete the token exchange, short enough that an
	// intercepted code is almost always already expired.
	oauthCodeTTL = 5 * time.Minute
	// oauthAccessTTL is the access token lifetime. Short, because the refresh token
	// rotates a new one without user interaction.
	oauthAccessTTL = time.Hour
	// oauthRefreshTTL bounds an idle connection: a client that has not refreshed
	// within this window must send the user back through consent.
	oauthRefreshTTL = 30 * 24 * time.Hour

	oauthCSRFCookie = "akari_oauth_csrf"
)

// baseURL resolves the externally reachable origin for this request. The
// configured public URL wins (the stable choice for the OAuth issuer); failing
// that it reconstructs the origin from the request, honoring a reverse proxy's
// X-Forwarded-Proto so the scheme is right behind TLS termination.
func (s *Server) baseURL(r *http.Request) string {
	if s.Cfg.PublicURL != "" {
		return s.Cfg.PublicURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	return scheme + "://" + r.Host
}

// handleProtectedResourceMetadata serves RFC 9728 metadata: it tells an MCP client
// which authorization server guards the /mcp resource, so the client knows where to
// register and authorize. akari is both the resource server and the authorization
// server, so the resource points back at this same origin.
func (s *Server) handleProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 base + mcpPath,
		"authorization_servers":    []string{base},
		"scopes_supported":         []string{scopeRead},
		"bearer_methods_supported": []string{"header"},
	})
}

// handleAuthServerMetadata serves RFC 8414 authorization-server metadata: the
// endpoints and capabilities a client needs to drive the flow. PKCE with S256 is
// the only challenge method, and clients authenticate as public clients (no
// secret), matching how the dynamic registration issues them.
func (s *Server) handleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	base := s.baseURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + oauthAuthorizePath,
		"token_endpoint":                        base + oauthTokenPath,
		"registration_endpoint":                 base + oauthRegisterPath,
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{scopeRead},
	})
}

// registerRequestBody is the subset of an RFC 7591 dynamic client registration we
// read. The rest of the client's metadata is accepted and ignored, so a client
// that sends grant_types, response_types, scope, and the like still registers.
type registerRequestBody struct {
	ClientName   string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
}

// handleOAuthRegister implements dynamic client registration (RFC 7591). MCP
// clients register themselves on first connect, so there is no console step. The
// client is public (PKCE, no secret), so registration stores only its name and its
// redirect allowlist and returns a generated client_id.
func (s *Server) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	var body registerRequestBody
	// Not decodeJSON: a registration carries many fields we deliberately ignore, and
	// DisallowUnknownFields would reject every compliant client.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client_metadata", "could not parse registration body")
		return
	}
	if len(body.RedirectURIs) == 0 {
		writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "at least one redirect_uri is required")
		return
	}
	for _, u := range body.RedirectURIs {
		if !validRedirectURI(u) {
			writeOAuthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri must be an absolute URI: "+u)
			return
		}
	}
	id, err := auth.NewToken()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not generate client id")
		return
	}
	if err := s.Store.CreateOAuthClient(r.Context(), id, strings.TrimSpace(body.ClientName), body.RedirectURIs); err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not store client")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  id,
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                body.ClientName,
		"redirect_uris":              body.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	})
}

// handleOAuthAuthorize is the authorization endpoint. It validates the request
// against the registered client, ensures the user is signed in (bouncing through
// the normal login page if not, which returns here afterward), and renders the
// consent screen. No code is issued here; approval posts back to the decision
// handler. Errors that cannot be safely redirected (an unknown client or an
// unregistered redirect) render an HTML error instead of bouncing to an
// attacker-chosen URL.
func (s *Server) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	setPrivateNoStore(w)
	q := r.URL.Query()
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")

	client, err := s.Store.OAuthClient(r.Context(), clientID)
	if errors.Is(err, store.ErrNotFound) {
		s.renderOAuthErrorPage(w, r, http.StatusBadRequest, "Unknown OAuth client. Re-register and try connecting again.")
		return
	} else if err != nil {
		s.renderOAuthErrorPage(w, r, http.StatusInternalServerError, "Could not load the OAuth client.")
		return
	}
	if !redirectRegistered(client, redirectURI) {
		s.renderOAuthErrorPage(w, r, http.StatusBadRequest, "The redirect URI is not registered for this client.")
		return
	}

	state := q.Get("state")
	if q.Get("response_type") != "code" {
		redirectOAuthError(w, r, redirectURI, state, "unsupported_response_type", "only response_type=code is supported")
		return
	}
	challenge := q.Get("code_challenge")
	if challenge == "" || q.Get("code_challenge_method") != "S256" {
		redirectOAuthError(w, r, redirectURI, state, "invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}

	// The browser must be signed in. resolve yields full scope for a session cookie;
	// if it is absent, send the user through login and return to this exact URL.
	p, ok := s.resolve(r)
	if !ok || p.Scope != scopeFull {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
		return
	}
	u, err := s.Store.UserByID(r.Context(), p.UserID)
	if err != nil {
		s.renderOAuthErrorPage(w, r, http.StatusInternalServerError, "Could not load your account.")
		return
	}

	csrf, err := auth.NewToken()
	if err != nil {
		s.renderOAuthErrorPage(w, r, http.StatusInternalServerError, "Could not start the consent flow.")
		return
	}
	s.setOAuthCSRFCookie(w, csrf)

	render(w, r, http.StatusOK, web.ConsentPage(web.Page{Title: "Connect"}, web.ConsentParams{
		ClientName:  clientDisplayName(client),
		Username:    u.Username,
		ClientID:    clientID,
		RedirectURI: redirectURI,
		State:       state,
		Challenge:   challenge,
		Resource:    q.Get("resource"),
		CSRF:        csrf,
	}))
}

// handleOAuthDecision processes the consent form. On approval it mints a
// single-use, PKCE-bound authorization code and redirects back to the client with
// it; on denial it redirects with an access_denied error. It re-validates the
// client and redirect (never trusting the posted values past what the cookie-bound
// session and the registration allow) and checks the double-submit CSRF token.
func (s *Server) handleOAuthDecision(w http.ResponseWriter, r *http.Request) {
	setPrivateNoStore(w)
	p, ok := s.resolve(r)
	if !ok || p.Scope != scopeFull {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderOAuthErrorPage(w, r, http.StatusBadRequest, "Malformed consent submission.")
		return
	}
	if !s.checkOAuthCSRF(r) {
		s.renderOAuthErrorPage(w, r, http.StatusBadRequest, "This consent form expired. Start the connection again.")
		return
	}
	s.clearOAuthCSRFCookie(w)

	clientID := r.PostFormValue("client_id")
	redirectURI := r.PostFormValue("redirect_uri")
	state := r.PostFormValue("state")

	client, err := s.Store.OAuthClient(r.Context(), clientID)
	if errors.Is(err, store.ErrNotFound) {
		s.renderOAuthErrorPage(w, r, http.StatusBadRequest, "Unknown client or unregistered redirect URI.")
		return
	} else if err != nil {
		s.renderOAuthErrorPage(w, r, http.StatusInternalServerError, "Could not load the OAuth client.")
		return
	}
	if !redirectRegistered(client, redirectURI) {
		s.renderOAuthErrorPage(w, r, http.StatusBadRequest, "Unknown client or unregistered redirect URI.")
		return
	}

	if r.PostFormValue("decision") != "approve" {
		redirectOAuthError(w, r, redirectURI, state, "access_denied", "the user declined the request")
		return
	}

	code, err := auth.NewToken()
	if err != nil {
		redirectOAuthError(w, r, redirectURI, state, "server_error", "could not issue an authorization code")
		return
	}
	ac := store.AuthCode{
		ClientID:      clientID,
		UserID:        p.UserID,
		RedirectURI:   redirectURI,
		CodeChallenge: r.PostFormValue("code_challenge"),
		Scope:         scopeRead,
		Resource:      r.PostFormValue("resource"),
	}
	if err := s.Store.CreateAuthCode(r.Context(), auth.HashToken(code), ac, time.Now().Add(oauthCodeTTL)); err != nil {
		redirectOAuthError(w, r, redirectURI, state, "server_error", "could not store the authorization code")
		return
	}

	redirectOAuthSuccess(w, r, redirectURI, state, code)
}

// handleOAuthToken is the token endpoint: it exchanges an authorization code for an
// access/refresh pair, or rotates a refresh token for a fresh pair. It speaks the
// form-encoded request and JSON response OAuth defines, with no-store caching so a
// proxy never retains a token.
func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		s.tokenFromCode(w, r)
	case "refresh_token":
		s.tokenFromRefresh(w, r)
	default:
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

func (s *Server) tokenFromCode(w http.ResponseWriter, r *http.Request) {
	code := r.PostFormValue("code")
	if code == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "code is required")
		return
	}
	codeHash := auth.HashToken(code)
	ac, err := s.Store.AuthCodeForExchange(r.Context(), codeHash)
	if errors.Is(err, store.ErrInvalidGrant) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid, expired, or already used")
		return
	} else if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not redeem the authorization code")
		return
	}
	if ac.ClientID != r.PostFormValue("client_id") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "client_id does not match the code")
		return
	}
	if ac.RedirectURI != r.PostFormValue("redirect_uri") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the code")
		return
	}
	if !verifyPKCE(ac.CodeChallenge, r.PostFormValue("code_verifier")) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}

	access, refresh, token, err := newOAuthToken(store.OAuthTokenParams{
		ClientID: ac.ClientID, UserID: ac.UserID, Scope: ac.Scope, Resource: ac.Resource,
	})
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not issue tokens")
		return
	}
	if err := s.Store.RedeemAuthCode(r.Context(), codeHash, token); errors.Is(err, store.ErrInvalidGrant) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid, expired, or already used")
		return
	} else if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not issue tokens")
		return
	}
	writeTokenResponse(w, access, refresh, ac.Scope)
}

func (s *Server) tokenFromRefresh(w http.ResponseWriter, r *http.Request) {
	rt := r.PostFormValue("refresh_token")
	if rt == "" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	access, err := auth.NewToken()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not issue tokens")
		return
	}
	refresh, err := auth.NewToken()
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not issue tokens")
		return
	}
	refreshExpiry := time.Now().Add(oauthRefreshTTL)
	_, _, scope, _, err := s.Store.RotateOAuthToken(r.Context(), auth.HashToken(rt), store.OAuthTokenParams{
		AccessHash:       auth.HashToken(access),
		RefreshHash:      auth.HashToken(refresh),
		AccessExpiresAt:  time.Now().Add(oauthAccessTTL),
		RefreshExpiresAt: &refreshExpiry,
	})
	if errors.Is(err, store.ErrInvalidGrant) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", "refresh token is invalid, expired, or revoked")
		return
	} else if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "could not rotate the refresh token")
		return
	}
	writeTokenResponse(w, access, refresh, scope)
}

// newOAuthToken mints a fresh access/refresh pair and prepares the hashes that are
// persisted by the caller. Keeping minting separate lets authorization-code
// redemption store the pair in the same transaction that consumes the code.
func newOAuthToken(p store.OAuthTokenParams) (access, refresh string, token store.OAuthTokenParams, err error) {
	access, err = auth.NewToken()
	if err != nil {
		return "", "", store.OAuthTokenParams{}, err
	}
	refresh, err = auth.NewToken()
	if err != nil {
		return "", "", store.OAuthTokenParams{}, err
	}
	refreshExpiry := time.Now().Add(oauthRefreshTTL)
	p.AccessHash = auth.HashToken(access)
	p.RefreshHash = auth.HashToken(refresh)
	p.AccessExpiresAt = time.Now().Add(oauthAccessTTL)
	p.RefreshExpiresAt = &refreshExpiry
	return access, refresh, p, nil
}

func writeTokenResponse(w http.ResponseWriter, access, refresh, scope string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(oauthAccessTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         scope,
	})
}

// verifyPKCE checks a code_verifier against the stored S256 challenge in constant
// time. An empty verifier never matches.
func verifyPKCE(challenge, verifier string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

// validRedirectURI accepts any absolute URI with a scheme and either a host or an
// opaque part, covering both the loopback http(s) redirects a CLI uses and the
// custom-scheme redirects a native client registers.
func validRedirectURI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return false
	}
	return u.Host != "" || u.Opaque != "" || u.Path != ""
}

// redirectRegistered reports whether uri exactly matches one of the client's
// registered redirect URIs. Exact match (not prefix) is deliberate: it is the
// allowlist that stops a stolen code from being sent to an attacker's endpoint.
func redirectRegistered(c store.OAuthClient, uri string) bool {
	for _, r := range c.RedirectURIs {
		if r == uri {
			return true
		}
	}
	return false
}

func clientDisplayName(c store.OAuthClient) string {
	if strings.TrimSpace(c.ClientName) != "" {
		return c.ClientName
	}
	return "An MCP client"
}

// redirectOAuthSuccess sends the browser back to the client with the code and the
// echoed state.
func redirectOAuthSuccess(w http.ResponseWriter, r *http.Request, redirectURI, state, code string) {
	dst := appendQuery(redirectURI, map[string]string{"code": code, "state": state})
	http.Redirect(w, r, dst, http.StatusSeeOther)
}

// redirectOAuthError sends the browser back to the client with an OAuth error and
// the echoed state, the spec's way to report a recoverable failure to the client.
func redirectOAuthError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, desc string) {
	dst := appendQuery(redirectURI, map[string]string{"error": code, "error_description": desc, "state": state})
	http.Redirect(w, r, dst, http.StatusSeeOther)
}

// appendQuery adds non-empty params to a URL, preserving any it already carries.
func appendQuery(raw string, params map[string]string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// writeOAuthError writes the JSON error envelope the token and registration
// endpoints use (RFC 6749 §5.2 / RFC 7591).
func writeOAuthError(w http.ResponseWriter, status int, code, desc string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

// renderOAuthErrorPage shows a browser-facing error for failures that must not
// redirect (an unknown client or an unregistered redirect), so a crafted authorize
// link cannot bounce the user to an attacker URL.
func (s *Server) renderOAuthErrorPage(w http.ResponseWriter, r *http.Request, status int, msg string) {
	render(w, r, status, web.ErrorPage(web.Page{Title: "Authorization error"}, status, msg))
}

func (s *Server) setOAuthCSRFCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     oauthCSRFCookie,
		Value:    value,
		Path:     "/oauth",
		HttpOnly: true,
		Secure:   s.Cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(oauthCodeTTL.Seconds()),
	})
}

func (s *Server) clearOAuthCSRFCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     oauthCSRFCookie,
		Value:    "",
		Path:     "/oauth",
		HttpOnly: true,
		Secure:   s.Cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// checkOAuthCSRF validates the double-submit token: the consent form's hidden field
// must match the cookie set when the form was rendered. Both ride the same
// same-origin, SameSite=Lax session, so a cross-site forgery carries neither.
func (s *Server) checkOAuthCSRF(r *http.Request) bool {
	c, err := r.Cookie(oauthCSRFCookie)
	if err != nil || c.Value == "" {
		return false
	}
	form := r.PostFormValue("csrf")
	return form != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(form)) == 1
}
