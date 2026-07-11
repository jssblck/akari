package main

import (
	"context"
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
