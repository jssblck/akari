package main

import (
	"path/filepath"
	"testing"
)

func TestResolveExecutableTargetFailsClosed(t *testing.T) {
	target := filepath.Join(t.TempDir(), "missing-akari-server")
	if resolved, err := resolveExecutableTarget(target); err == nil {
		t.Fatalf("resolveExecutableTarget(%q) = %q, nil; want an error", target, resolved)
	}
}
