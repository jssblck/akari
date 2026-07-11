package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jssblck/akari/internal/client/daemon"
)

func TestDaemonStatusCommandPropagatesProbeErrors(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("AppData", configHome)
	t.Setenv("HOME", configHome)
	paths, err := daemon.DefaultPaths()
	if err != nil {
		t.Fatalf("default daemon paths: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.Pidfile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(paths.Pidfile, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := runDaemon([]string{"status"}); err == nil {
		t.Fatal("daemon status command swallowed pidfile probe error")
	}
}

func TestDaemonStopCommandRejectsInvalidTimeout(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("AppData", configHome)
	t.Setenv("HOME", configHome)

	err := runDaemon([]string{"stop", "--timeout=-1s"})
	if err == nil || errors.Is(err, daemon.ErrNotRunning) {
		t.Fatalf("daemon stop error = %v, want timeout validation", err)
	}
}

func TestDaemonStartRejectsStopOnlyOptions(t *testing.T) {
	if err := runDaemon([]string{"start", "--force"}); err == nil {
		t.Fatal("daemon start accepted --force")
	}
}
