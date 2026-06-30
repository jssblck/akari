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
}

// LoadServer reads server configuration from the environment, applying defaults
// and validating required values.
func LoadServer() (Server, error) {
	s := Server{
		DatabaseURL:  os.Getenv("AKARI_DATABASE_URL"),
		Listen:       listenAddr(),
		CookieSecure: !truthy(os.Getenv("AKARI_COOKIE_INSECURE")),
		PublicURL:    publicURL(),
	}
	if s.DatabaseURL == "" {
		return Server{}, fmt.Errorf("AKARI_DATABASE_URL is required")
	}
	interval, err := parseDuration(os.Getenv("AKARI_SWEEP_INTERVAL"), time.Hour)
	if err != nil {
		return Server{}, fmt.Errorf("AKARI_SWEEP_INTERVAL: %w", err)
	}
	s.SweepInterval = interval
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

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
