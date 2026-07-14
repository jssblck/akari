package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/jssblck/akari/internal/config"
)

// Path-prefix support: akari can be served behind a reverse proxy under an
// arbitrary external path (https://host/proxy/akari/...). The prefix comes
// from the path component of AKARI_PUBLIC_URL, or per request from a trusted
// header the proxy sets (AKARI_PREFIX_HEADER, typically X-Forwarded-Prefix).
//
// Internally every route stays rooted: withPathPrefix resolves the prefix
// once, stashes it in the request context, and strips it from the incoming
// path when present, so the proxy may forward paths stripped or unstripped
// and the mux patterns never change. Everything the server generates that a
// client resolves externally (redirects, cookie Path scopes, URLs in served
// HTML and discovery documents) is built back through href/absURL.

const prefixKey ctxKey = iota + 100

// requestPrefix returns the external path prefix resolved for this request:
// "" for a root deployment, otherwise a normalized "/like/this".
func requestPrefix(r *http.Request) string {
	p, _ := r.Context().Value(prefixKey).(string)
	return p
}

// resolvePrefix picks the request's external prefix: a valid trusted-header
// value wins, then the static configured prefix. A configured-but-absent or
// invalid header falls back rather than failing: the header names how this
// particular request was reached, and a request that arrives without it (a
// direct health check, a stripped-prefix hop) is simply a root-path request.
// The header gets the same shared-secret defense in depth the identity header
// gets, so a client that reaches akari directly cannot skew generated URLs or
// cookie scopes.
func (s *Server) resolvePrefix(r *http.Request) string {
	if s.Cfg.PrefixHeader != "" {
		if v := strings.TrimSpace(r.Header.Get(s.Cfg.PrefixHeader)); v != "" && s.proxySecretPresented(r) {
			if p, err := config.NormalizePathPrefix(v); err == nil {
				return p
			}
		}
	}
	return s.Cfg.PathPrefix
}

// withPathPrefix resolves the request's external prefix into the context and
// strips it from the URL path when the proxy forwarded it unstripped, so the
// rooted mux patterns match either proxy style. It wraps the whole router and
// is a no-op for a root deployment.
func (s *Server) withPathPrefix(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix := s.resolvePrefix(r)
		if prefix == "" {
			next.ServeHTTP(w, r)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), prefixKey, prefix))
		if stripped, ok := stripPrefix(r.URL.Path, prefix); ok {
			r.URL.Path = stripped
			// RawPath only exists when the escaped form differs from Path. The
			// prefix grammar admits no percent-escapes, so when a RawPath is
			// present its head is byte-identical to the prefix and strips the
			// same way; a mismatched RawPath is stale after editing Path, so it
			// must be dropped rather than kept.
			if r.URL.RawPath != "" {
				if rawStripped, rawOK := stripPrefix(r.URL.RawPath, prefix); rawOK {
					r.URL.RawPath = rawStripped
				} else {
					r.URL.RawPath = ""
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// stripPrefix removes an exact leading prefix from a path, reporting whether
// it applied. The bare prefix itself maps to "/", and a prefix is only
// stripped on a segment boundary so "/proxy/akari-other" is not mangled by a
// "/proxy/akari" prefix.
func stripPrefix(path, prefix string) (string, bool) {
	if path == prefix {
		return "/", true
	}
	if strings.HasPrefix(path, prefix+"/") {
		return path[len(prefix):], true
	}
	return "", false
}

// href externalizes a rooted path for this request: the resolved prefix plus
// the path. Every Location header and every path written into a served page
// must pass through here (or absURL), because the browser resolves them
// against the external URL space, not the stripped internal one.
func (s *Server) href(r *http.Request, path string) string {
	return requestPrefix(r) + path
}

// absURL is href with the externally reachable origin in front: the base for
// canonical links, Open Graph tags, and the OAuth/MCP discovery documents.
func (s *Server) absURL(r *http.Request, path string) string {
	return s.baseURL(r) + requestPrefix(r) + path
}

// cookiePath is the Path attribute for cookies that scope to the whole app:
// the resolved prefix, or "/" at the root. Scoping to the prefix keeps the
// session and CSRF cookies from being presented to unrelated applications
// sharing the same external origin.
func cookiePath(r *http.Request) string {
	if p := requestPrefix(r); p != "" {
		return p
	}
	return "/"
}
