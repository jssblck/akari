package config

import (
	"testing"
	"time"
)

func TestLoadServerAnalyticsSnapshotPolicy(t *testing.T) {
	t.Setenv("AKARI_DATABASE_URL", "postgres://x/y")
	t.Setenv("AKARI_ANALYTICS_SNAPSHOT_FRESHNESS", "")
	t.Setenv("AKARI_ANALYTICS_SNAPSHOT_STALE_FOR", "")
	t.Setenv("AKARI_ANALYTICS_SNAPSHOT_LIMIT", "")

	s, err := LoadServer()
	if err != nil {
		t.Fatalf("LoadServer defaults: %v", err)
	}
	if s.AnalyticsSnapshotFreshness != time.Minute || s.AnalyticsSnapshotStaleFor != 15*time.Minute || s.AnalyticsSnapshotLimit != 256 {
		t.Fatalf("defaults = (%v, %v, %d), want (1m, 15m, 256)", s.AnalyticsSnapshotFreshness, s.AnalyticsSnapshotStaleFor, s.AnalyticsSnapshotLimit)
	}

	t.Setenv("AKARI_ANALYTICS_SNAPSHOT_FRESHNESS", "30s")
	t.Setenv("AKARI_ANALYTICS_SNAPSHOT_STALE_FOR", "2m")
	t.Setenv("AKARI_ANALYTICS_SNAPSHOT_LIMIT", "17")
	s, err = LoadServer()
	if err != nil {
		t.Fatalf("LoadServer explicit policy: %v", err)
	}
	if s.AnalyticsSnapshotFreshness != 30*time.Second || s.AnalyticsSnapshotStaleFor != 2*time.Minute || s.AnalyticsSnapshotLimit != 17 {
		t.Fatalf("explicit = (%v, %v, %d), want (30s, 2m, 17)", s.AnalyticsSnapshotFreshness, s.AnalyticsSnapshotStaleFor, s.AnalyticsSnapshotLimit)
	}
}

func TestLoadServerRejectsInvalidAnalyticsSnapshotPolicy(t *testing.T) {
	t.Setenv("AKARI_DATABASE_URL", "postgres://x/y")
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{name: "zero freshness", key: "AKARI_ANALYTICS_SNAPSHOT_FRESHNESS", value: "0"},
		{name: "malformed freshness", key: "AKARI_ANALYTICS_SNAPSHOT_FRESHNESS", value: "soon"},
		{name: "negative stale interval", key: "AKARI_ANALYTICS_SNAPSHOT_STALE_FOR", value: "-1m"},
		{name: "zero entry limit", key: "AKARI_ANALYTICS_SNAPSHOT_LIMIT", value: "0"},
		{name: "malformed entry limit", key: "AKARI_ANALYTICS_SNAPSHOT_LIMIT", value: "many"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AKARI_ANALYTICS_SNAPSHOT_FRESHNESS", "")
			t.Setenv("AKARI_ANALYTICS_SNAPSHOT_STALE_FOR", "")
			t.Setenv("AKARI_ANALYTICS_SNAPSHOT_LIMIT", "")
			t.Setenv(tc.key, tc.value)
			if _, err := LoadServer(); err == nil {
				t.Fatalf("LoadServer accepted %s=%q", tc.key, tc.value)
			}
		})
	}
}
