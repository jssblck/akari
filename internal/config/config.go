// Package config loads akari-server configuration.
//
// The server is a container workload, so it reads its configuration from the
// environment by convention. (The akari client, by contrast, uses a config file
// and defines no environment variables of its own; see docs/DESIGN.md.)
package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Server holds the akari-server runtime configuration.
type Server struct {
	// DatabaseURL is the Postgres connection string (AKARI_DATABASE_URL).
	DatabaseURL string
	// Listen is the address the HTTP server binds (AKARI_LISTEN), e.g. ":8080".
	Listen string
	// CookieSecure marks session cookies Secure. Defaults true; set
	// AKARI_COOKIE_INSECURE=1 for plain-HTTP local development.
	CookieSecure bool
	// PublicURL is the externally reachable base URL of the server (scheme and
	// host, no trailing slash), e.g. "https://akari.example.com". It is the OAuth
	// issuer and the base of every absolute URL the MCP authorization flow
	// advertises (the discovery documents, the authorize and token endpoints, the
	// MCP resource identifier). Read from AKARI_PUBLIC_URL, falling back to
	// AKARI_URL (which eph already exports for the dev stack). When neither is set,
	// it is empty and the OAuth handlers derive the base from each request's scheme
	// and Host header, which is correct for a single-origin deployment behind a
	// well-behaved proxy.
	PublicURL string
	// SweepInterval is how often the server reclaims orphaned CAS blobs
	// (AKARI_SWEEP_INTERVAL, a Go duration like "1h"). Defaults to 1h; set "0" to
	// disable the background sweep (for example to run it only via the subcommand).
	SweepInterval time.Duration
	// OGCacheTTL is how long a rendered Open Graph preview card is served from the
	// cache before the next request re-renders it (AKARI_OG_CACHE_TTL, a Go duration
	// like "1h"). A published overview's card is rendered lazily on first request and
	// cached; once it ages past this TTL a later request renders a fresh one in its
	// place. Defaults to 1h and must be positive (the card is always cached).
	OGCacheTTL time.Duration
	// OGCleanupInterval is how often the server sweeps expired preview cards from the
	// cache (AKARI_OG_CLEANUP_INTERVAL, a Go duration). Each pass deletes cards older
	// than OGCacheTTL, so a card for an overview nobody shares does not linger. It is
	// pure housekeeping: a live share re-renders its card on demand regardless.
	// Defaults to 24h; set "0" to disable the sweep.
	OGCleanupInterval time.Duration
	// ProxyAuthHeader is the request header a trusted reverse proxy sets to the
	// authenticated username (AKARI_PROXY_AUTH_HEADER, e.g.
	// "X-Auth-Request-Preferred-Username"). Setting it turns on proxy-header auth:
	// the server trusts the header's value as the signed-in user and provisions the
	// account on first sight. It is empty (disabled) by default, because trusting a
	// header is only safe when akari is reachable ONLY through the proxy that sets
	// it. Leave it unset for a direct deployment. See auth.go's proxyPrincipal and
	// the deployment notes in docs/development.md.
	ProxyAuthHeader string
	// ProxyAuthSecret, when set (AKARI_PROXY_AUTH_SECRET), is a shared secret the
	// proxy must echo in ProxyAuthSecretHeader for the identity header to be
	// trusted. It is defense in depth for when network isolation alone is not
	// enough: a client that reaches akari directly cannot forge an identity without
	// also knowing the secret. Empty means the identity header is trusted on
	// network isolation alone.
	ProxyAuthSecret string
	// ProxyAuthSecretHeader is the header carrying ProxyAuthSecret
	// (AKARI_PROXY_AUTH_SECRET_HEADER). Defaults to "X-Akari-Proxy-Secret". Only
	// consulted when ProxyAuthSecret is set.
	ProxyAuthSecretHeader string
	// SignalsSettleInterval is how often the server wakes to compute per-session
	// signals for sessions that have settled (AKARI_SIGNALS_SETTLE_INTERVAL). The
	// ingest append path deliberately does not recompute signals per message (that
	// would be quadratic and would grade a still-running session with a
	// time-dependent outcome), so a settled session's grade is filled in here, once,
	// after it has been idle past the abandoned threshold. Defaults to 5m; set "0"
	// to disable the background pass (signals then land only on reparse or via the
	// subcommand).
	SignalsSettleInterval time.Duration
}

// LoadServer reads server configuration from the environment, applying defaults
// and validating required values.
func LoadServer() (Server, error) {
	s := Server{
		DatabaseURL:           os.Getenv("AKARI_DATABASE_URL"),
		Listen:                listenAddr(),
		CookieSecure:          !truthy(os.Getenv("AKARI_COOKIE_INSECURE")),
		PublicURL:             publicURL(),
		ProxyAuthHeader:       strings.TrimSpace(os.Getenv("AKARI_PROXY_AUTH_HEADER")),
		ProxyAuthSecret:       os.Getenv("AKARI_PROXY_AUTH_SECRET"),
		ProxyAuthSecretHeader: proxyAuthSecretHeader(),
	}
	if s.DatabaseURL == "" {
		return Server{}, fmt.Errorf("AKARI_DATABASE_URL is required")
	}
	interval, err := parseDuration(os.Getenv("AKARI_SWEEP_INTERVAL"), time.Hour)
	if err != nil {
		return Server{}, fmt.Errorf("AKARI_SWEEP_INTERVAL: %w", err)
	}
	s.SweepInterval = interval
	ttl, err := parseDuration(os.Getenv("AKARI_OG_CACHE_TTL"), time.Hour)
	if err != nil {
		return Server{}, fmt.Errorf("AKARI_OG_CACHE_TTL: %w", err)
	}
	if ttl <= 0 {
		return Server{}, fmt.Errorf("AKARI_OG_CACHE_TTL must be positive")
	}
	s.OGCacheTTL = ttl
	cleanup, err := parseDuration(os.Getenv("AKARI_OG_CLEANUP_INTERVAL"), 24*time.Hour)
	if err != nil {
		return Server{}, fmt.Errorf("AKARI_OG_CLEANUP_INTERVAL: %w", err)
	}
	s.OGCleanupInterval = cleanup
	settleInterval, err := parseDuration(os.Getenv("AKARI_SIGNALS_SETTLE_INTERVAL"), 5*time.Minute)
	if err != nil {
		return Server{}, fmt.Errorf("AKARI_SIGNALS_SETTLE_INTERVAL: %w", err)
	}
	s.SignalsSettleInterval = settleInterval
	return s, nil
}

// parseDuration reads a Go duration string, returning fallback when unset and
// allowing "0" to mean disabled.
func parseDuration(v string, fallback time.Duration) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, fmt.Errorf("must not be negative")
	}
	return d, nil
}

// listenAddr resolves the HTTP bind address. AKARI_LISTEN wins when set. Failing
// that it honors PORT (the convention many process supervisors and dev tools,
// including this repo's preview launcher, use to hand a process its assigned
// port), binding all interfaces on that port. The final fallback is :8080.
func listenAddr() string {
	if v := os.Getenv("AKARI_LISTEN"); v != "" {
		return v
	}
	if p := os.Getenv("PORT"); p != "" {
		return ":" + p
	}
	return ":8080"
}

// publicURL resolves the server's externally reachable base URL. AKARI_PUBLIC_URL
// wins; failing that it honors AKARI_URL, the variable eph exports pointing at the
// running dev server, so the OAuth flow works out of the box under eph. The result
// is trimmed of any trailing slash so callers can join paths with a leading slash
// without doubling it. An empty result tells the OAuth handlers to derive the base
// from the request instead.
func publicURL() string {
	v := strings.TrimSpace(os.Getenv("AKARI_PUBLIC_URL"))
	if v == "" {
		v = strings.TrimSpace(os.Getenv("AKARI_URL"))
	}
	return strings.TrimRight(v, "/")
}

// proxyAuthSecretHeader resolves the header the proxy echoes the shared secret in,
// defaulting to X-Akari-Proxy-Secret when unset so operators need only set the
// secret value itself in the common case.
func proxyAuthSecretHeader() string {
	if v := strings.TrimSpace(os.Getenv("AKARI_PROXY_AUTH_SECRET_HEADER")); v != "" {
		return v
	}
	return "X-Akari-Proxy-Secret"
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
