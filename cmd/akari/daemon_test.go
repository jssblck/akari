package main

import (
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
