package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveUpdateTargetFailsClosed(t *testing.T) {
	if _, err := resolveUpdateTarget(filepath.Join(t.TempDir(), "missing-akari")); err == nil {
		t.Fatal("resolveUpdateTarget accepted an executable path it could not resolve")
	}
}

func TestResolveUpdateTargetReturnsExistingFile(t *testing.T) {
	target := filepath.Join(t.TempDir(), "akari")
	if err := os.WriteFile(target, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveUpdateTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	wantInfo, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	gotInfo, err := os.Stat(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(gotInfo, wantInfo) {
		t.Fatalf("resolved target %q is not the original executable", resolved)
	}
}
