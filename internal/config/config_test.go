package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadServerPublicOrigin(t *testing.T) {
	t.Setenv("AKARI_DATABASE_URL", "postgres://x/y")
	t.Setenv("AKARI_URL", "")

	for _, tc := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "canonical", raw: "https://akari.example", want: "https://akari.example"},
		{name: "case and default port", raw: "HTTPS://AKARI.EXAMPLE:443/", want: "https://akari.example"},
		{name: "nondefault port", raw: "http://localhost:8080", want: "http://localhost:8080"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AKARI_PUBLIC_URL", tc.raw)
			cfg, err := LoadServer()
			if err != nil {
				t.Fatalf("LoadServer: %v", err)
			}
			if cfg.PublicURL != tc.want {
				t.Fatalf("PublicURL = %q, want %q", cfg.PublicURL, tc.want)
			}
		})
	}

	for _, raw := range []string{
		"akari.example",
		"ftp://akari.example",
		"https://akari.example/path",
		"https://akari.example?query=1",
		"https://user@akari.example",
		"https://akari.example#fragment",
	} {
		t.Run("reject "+raw, func(t *testing.T) {
			t.Setenv("AKARI_PUBLIC_URL", raw)
			if _, err := LoadServer(); err == nil || !strings.Contains(err.Error(), "AKARI_PUBLIC_URL") {
				t.Fatalf("LoadServer(%q) error = %v, want AKARI_PUBLIC_URL error", raw, err)
			}
		})
	}
}

// TestLoadServerPublicOriginFallbackLabel confirms a validation failure names
// whichever variable actually supplied the value. An operator who never set
// AKARI_PUBLIC_URL, only the AKARI_URL eph exports, should see AKARI_URL
// named in the error, not the unset variable.
func TestLoadServerPublicOriginFallbackLabel(t *testing.T) {
	t.Setenv("AKARI_DATABASE_URL", "postgres://x/y")
	t.Setenv("AKARI_PUBLIC_URL", "")
	t.Setenv("AKARI_URL", "akari.example")

	_, err := LoadServer()
	if err == nil || !strings.Contains(err.Error(), "AKARI_URL") {
		t.Fatalf("LoadServer() error = %v, want AKARI_URL error", err)
	}
	if strings.Contains(err.Error(), "AKARI_PUBLIC_URL") {
		t.Fatalf("LoadServer() error = %v, wrongly names AKARI_PUBLIC_URL for an AKARI_URL value", err)
	}
}

func TestLoadServerOGCacheTTL(t *testing.T) {
	t.Setenv("AKARI_DATABASE_URL", "postgres://x/y")
	t.Setenv("AKARI_PUBLIC_URL", "")
	t.Setenv("AKARI_URL", "")
	// Isolate from any ambient value the harness may export.
	t.Setenv("AKARI_OG_CACHE_TTL", "")

	// Default when unset.
	s, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if s.OGCacheTTL != time.Hour {
		t.Fatalf("default OGCacheTTL = %v, want 1h", s.OGCacheTTL)
	}

	// Explicit duration.
	t.Setenv("AKARI_OG_CACHE_TTL", "15m")
	if s, err = LoadServer(); err != nil || s.OGCacheTTL != 15*time.Minute {
		t.Fatalf("OGCacheTTL = %v (err %v), want 15m", s.OGCacheTTL, err)
	}

	// The card is always cached, so a non-positive TTL is a load error, not a silent
	// "never cache".
	t.Setenv("AKARI_OG_CACHE_TTL", "0")
	if _, err = LoadServer(); err == nil {
		t.Fatal("LoadServer accepted a zero AKARI_OG_CACHE_TTL")
	}

	// A malformed value is a load error, not a silent default.
	t.Setenv("AKARI_OG_CACHE_TTL", "banana")
	if _, err = LoadServer(); err == nil {
		t.Fatal("LoadServer accepted a malformed AKARI_OG_CACHE_TTL")
	}
}

func TestLoadServerOGCleanupInterval(t *testing.T) {
	t.Setenv("AKARI_DATABASE_URL", "postgres://x/y")
	t.Setenv("AKARI_PUBLIC_URL", "")
	t.Setenv("AKARI_URL", "")
	// Isolate from any ambient values the harness may export.
	t.Setenv("AKARI_OG_CACHE_TTL", "")
	t.Setenv("AKARI_OG_CLEANUP_INTERVAL", "")

	// Default when unset.
	s, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if s.OGCleanupInterval != 24*time.Hour {
		t.Fatalf("default OGCleanupInterval = %v, want 24h", s.OGCleanupInterval)
	}

	// Explicit duration.
	t.Setenv("AKARI_OG_CLEANUP_INTERVAL", "6h")
	if s, err = LoadServer(); err != nil || s.OGCleanupInterval != 6*time.Hour {
		t.Fatalf("OGCleanupInterval = %v (err %v), want 6h", s.OGCleanupInterval, err)
	}

	// "0" disables the sweep.
	t.Setenv("AKARI_OG_CLEANUP_INTERVAL", "0")
	if s, err = LoadServer(); err != nil || s.OGCleanupInterval != 0 {
		t.Fatalf("OGCleanupInterval = %v (err %v), want 0", s.OGCleanupInterval, err)
	}

	// A malformed value is a load error, not a silent default.
	t.Setenv("AKARI_OG_CLEANUP_INTERVAL", "banana")
	if _, err = LoadServer(); err == nil {
		t.Fatal("LoadServer accepted a malformed AKARI_OG_CLEANUP_INTERVAL")
	}
}

func TestLoadServerInsightsRefreshInterval(t *testing.T) {
	t.Setenv("AKARI_DATABASE_URL", "postgres://x/y")
	t.Setenv("AKARI_PUBLIC_URL", "")
	t.Setenv("AKARI_URL", "")
	// Isolate from any ambient value the harness may export.
	t.Setenv("AKARI_INSIGHTS_REFRESH_INTERVAL", "")

	// Default when unset.
	s, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if s.InsightsRefreshInterval != time.Hour {
		t.Fatalf("default InsightsRefreshInterval = %v, want 1h", s.InsightsRefreshInterval)
	}

	// Explicit duration.
	t.Setenv("AKARI_INSIGHTS_REFRESH_INTERVAL", "15m")
	if s, err = LoadServer(); err != nil || s.InsightsRefreshInterval != 15*time.Minute {
		t.Fatalf("InsightsRefreshInterval = %v (err %v), want 15m", s.InsightsRefreshInterval, err)
	}

	// "0" disables the background loop.
	t.Setenv("AKARI_INSIGHTS_REFRESH_INTERVAL", "0")
	if s, err = LoadServer(); err != nil || s.InsightsRefreshInterval != 0 {
		t.Fatalf("InsightsRefreshInterval = %v (err %v), want 0", s.InsightsRefreshInterval, err)
	}

	// A malformed value is a load error, not a silent default.
	t.Setenv("AKARI_INSIGHTS_REFRESH_INTERVAL", "banana")
	if _, err = LoadServer(); err == nil {
		t.Fatal("LoadServer accepted a malformed AKARI_INSIGHTS_REFRESH_INTERVAL")
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		in       string
		fallback time.Duration
		want     time.Duration
		wantErr  bool
	}{
		{"", time.Hour, time.Hour, false},
		{"30m", time.Hour, 30 * time.Minute, false},
		{"0", time.Hour, 0, false},
		{"  2h  ", time.Hour, 2 * time.Hour, false},
		{"-5m", time.Hour, 0, true},
		{"banana", time.Hour, 0, true},
	}
	for _, c := range cases {
		got, err := parseDuration(c.in, c.fallback)
		if (err != nil) != c.wantErr {
			t.Errorf("parseDuration(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("parseDuration(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
