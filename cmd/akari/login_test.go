package main

import (
	"path/filepath"
	"testing"

	"github.com/jssblck/akari/internal/config"
)

// TestLoginMachineFlag pins the three states of `--machine`: setting a name,
// leaving an existing name untouched on a re-run that omits the flag, and
// clearing it back to the hostname with an explicit empty value.
func TestLoginMachineFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")

	// First login sets a machine name.
	if err := runLogin([]string{"--server", "https://a", "--token", "t1", "--machine", "ci", "--config", path}); err != nil {
		t.Fatal(err)
	}
	if got := readMachine(t, path); got != "ci" {
		t.Fatalf("after set, machine = %q, want ci", got)
	}

	// Re-running without --machine rotates the token but preserves the machine.
	if err := runLogin([]string{"--server", "https://a", "--token", "t2", "--config", path}); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadClient(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Machine != "ci" {
		t.Errorf("machine not preserved across re-login: %q", cfg.Machine)
	}
	if cfg.Token != "t2" {
		t.Errorf("token not rotated: %q", cfg.Token)
	}

	// Passing --machine with an empty value explicitly clears it.
	if err := runLogin([]string{"--server", "https://a", "--token", "t2", "--machine", "", "--config", path}); err != nil {
		t.Fatal(err)
	}
	if got := readMachine(t, path); got != "" {
		t.Errorf("after clear, machine = %q, want empty", got)
	}
}

func readMachine(t *testing.T, path string) string {
	t.Helper()
	cfg, err := config.LoadClient(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Machine
}
