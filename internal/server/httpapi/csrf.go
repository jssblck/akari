package httpapi

import (
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/auth"
)

const (
	csrfCookieName = "akari_csrf"
	csrfHeaderName = "X-Akari-CSRF-Token"
	csrfFormName   = "_csrf"
)

// withRouteCSRF lets ServeMux retain ownership of unknown paths and unsupported
// methods. mux.Handler reports an empty pattern both when no route matches and
// when a route matches but not this method; Go's ServeMux does not distinguish
// the two through this call. Either way the request falls through to next, the
// styled not-found handler, which does its own 404/405 split, so a request that
// cannot reach a mutation handler keeps its normal response instead of being
// masked by the CSRF gate.
func (s *Server) withRouteCSRF(mux *http.ServeMux, next http.Handler) http.Handler {
	protected := s.withCSRF(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isSafeMethod(r.Method) {
			_, pattern := mux.Handler(r)
			if pattern == "" {
				next.ServeHTTP(w, r)
				return
			}
		}
		protected.ServeHTTP(w, r)
	})
}

// withCSRF enforces the browser trust boundary before unsafe handlers run. OAuth
// protocol and MCP endpoints do not authenticate from the browser session, and
// an explicit Bearer credential commits auth resolution to that token, so those
// requests stay outside the cookie CSRF mechanism.
func (s *Server) withCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSafeMethod(r.Method) {
			if s.csrfTokenPage(r) {
				if !s.ensureCSRFCookie(w, r) {
					writeError(w, http.StatusInternalServerError, "could not initialize CSRF protection")
					return
				}
			}
			next.ServeHTTP(w, r)
			return
		}
		if csrfExempt(r) {
			next.ServeHTTP(w, r)
			return
		}

		token, hadToken := csrfTokenFromRequest(r)
		if !hadToken {
			var err error
			token, err = auth.NewToken()
			if err != nil {
				writeError(w, http.StatusInternalServerError, "could not initialize CSRF protection")
				return
			}
			s.setCSRFCookie(w, r, token)
		}
		browserSignal, ok := s.validBrowserOrigin(r)
		if !ok || (!browserSignal && (!hadToken || !validCSRFToken(r, token))) {
			writeError(w, http.StatusForbidden, "CSRF validation failed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func csrfExempt(r *http.Request) bool {
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		return true
	}
	switch r.URL.Path {
	case oauthRegisterPath, oauthTokenPath, mcpPath:
		return true
	default:
		return false
	}
}

// csrfTokenPage limits token cookies to pages that can render a mutation form.
// Public, cacheable pages stay free of per-viewer Set-Cookie headers.
func (s *Server) csrfTokenPage(r *http.Request) bool {
	if r.URL.Path == "/login" || r.URL.Path == "/register" || r.URL.Path == oauthAuthorizePath {
		return true
	}
	if _, err := r.Cookie(cookieName); err == nil {
		return true
	}
	_, asserted := s.proxyIdentity(r)
	return asserted
}

func (s *Server) ensureCSRFCookie(w http.ResponseWriter, r *http.Request) bool {
	_, ok := csrfTokenFromRequest(r)
	if !ok {
		token, err := auth.NewToken()
		if err != nil {
			return false
		}
		s.setCSRFCookie(w, r, token)
	}
	return true
}

func (s *Server) setCSRFCookie(w http.ResponseWriter, r *http.Request, token string) {
	setPrivateNoStore(w)
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookieName,
		Value:    token,
		Path:     cookiePath(r),
		HttpOnly: true,
		Secure:   s.Cfg.CookieSecure,
		SameSite: http.SameSiteStrictMode,
		Expires:  time.Now().Add(sessionTTL),
	})
}

// rotateCSRFCookie mints a fresh double-submit token and installs it in place
// of whatever token predates the call. Login, registration, and logout use it
// so a token issued before the privilege change cannot go on authorizing
// requests under the new session state.
func (s *Server) rotateCSRFCookie(w http.ResponseWriter, r *http.Request) error {
	token, err := auth.NewToken()
	if err != nil {
		return err
	}
	s.setCSRFCookie(w, r, token)
	return nil
}

func csrfTokenFromRequest(r *http.Request) (string, bool) {
	c, err := r.Cookie(csrfCookieName)
	if err != nil || !wellFormedCSRFToken(c.Value) {
		return "", false
	}
	return c.Value, true
}

func wellFormedCSRFToken(token string) bool {
	b, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(b) == 32
}

func validCSRFToken(r *http.Request, cookieToken string) bool {
	presented := r.Header.Get(csrfHeaderName)
	if presented == "" {
		presented = r.PostFormValue(csrfFormName)
	}
	if !wellFormedCSRFToken(presented) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(cookieToken)) == 1
}

// validBrowserOrigin validates every browser-controlled origin signal that is
// present. It returns whether at least one signal was supplied, and rejects a
// mismatch or malformed/conflicting metadata even when a fallback token exists.
func (s *Server) validBrowserOrigin(r *http.Request) (bool, bool) {
	hadSignal := false
	origins := r.Header.Values("Origin")
	if len(origins) > 0 {
		hadSignal = true
		if len(origins) != 1 || strings.Contains(origins[0], ",") {
			return true, false
		}
		got, err := config.NormalizePublicOrigin(strings.TrimSpace(origins[0]))
		if err != nil {
			return true, false
		}
		want, err := config.NormalizePublicOrigin(s.baseURL(r))
		if err != nil || got != want {
			return true, false
		}
	}

	fetchSites := r.Header.Values("Sec-Fetch-Site")
	if len(fetchSites) > 0 {
		hadSignal = true
		if len(fetchSites) != 1 || strings.TrimSpace(fetchSites[0]) != "same-origin" {
			return true, false
		}
	}
	return hadSignal, true
}
