package config

import (
	"testing"
	"time"
)

func TestLoadServerOGRefreshInterval(t *testing.T) {
	t.Setenv("AKARI_DATABASE_URL", "postgres://x/y")
	// Isolate from any ambient value the harness may export.
	t.Setenv("AKARI_OG_REFRESH_INTERVAL", "")

	// Default when unset.
	s, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer: %v", err)
	}
	if s.OGRefreshInterval != time.Hour {
		t.Fatalf("default OGRefreshInterval = %v, want 1h", s.OGRefreshInterval)
	}

	// Explicit duration.
	t.Setenv("AKARI_OG_REFRESH_INTERVAL", "15m")
	if s, err = LoadServer(); err != nil || s.OGRefreshInterval != 15*time.Minute {
		t.Fatalf("OGRefreshInterval = %v (err %v), want 15m", s.OGRefreshInterval, err)
	}

	// "0" disables.
	t.Setenv("AKARI_OG_REFRESH_INTERVAL", "0")
	if s, err = LoadServer(); err != nil || s.OGRefreshInterval != 0 {
		t.Fatalf("OGRefreshInterval = %v (err %v), want 0", s.OGRefreshInterval, err)
	}

	// A malformed value is a load error, not a silent default.
	t.Setenv("AKARI_OG_REFRESH_INTERVAL", "banana")
	if _, err = LoadServer(); err == nil {
		t.Fatal("LoadServer accepted a malformed AKARI_OG_REFRESH_INTERVAL")
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
