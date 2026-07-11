package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestDaemonWatchRecordsStartupErrorsInRotatingLog(t *testing.T) {
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("AppData", configHome)
	t.Setenv("HOME", configHome)
	logPath := filepath.Join(configHome, "daemon.log")

	err := runWatch(context.Background(), []string{
		"--config", filepath.Join(configHome, "missing.toml"),
		"--daemon-log", logPath,
	})
	if err == nil {
		t.Fatal("daemon watch unexpectedly accepted a missing config")
	}
	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(data), "akari: "+err.Error()) {
		t.Fatalf("daemon log %q does not contain startup error %q", data, err)
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
