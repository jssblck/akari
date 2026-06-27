// Package config loads akari-server configuration.
//
// The server is a container workload, so it reads its configuration from the
// environment by convention. (The akari client, by contrast, uses a config file
// and defines no environment variables of its own; see DESIGN.md.)
package config

import (
	"fmt"
	"os"
	"strings"
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
}

// LoadServer reads server configuration from the environment, applying defaults
// and validating required values.
func LoadServer() (Server, error) {
	s := Server{
		DatabaseURL:  os.Getenv("AKARI_DATABASE_URL"),
		Listen:       envOr("AKARI_LISTEN", ":8080"),
		CookieSecure: !truthy(os.Getenv("AKARI_COOKIE_INSECURE")),
	}
	if s.DatabaseURL == "" {
		return Server{}, fmt.Errorf("AKARI_DATABASE_URL is required")
	}
	return s, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
