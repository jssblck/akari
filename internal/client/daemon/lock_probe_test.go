package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestIsRunningDoesNotCreateOrRewritePidfile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	path := filepath.Join(dir, "akari.pid")
	running, err := IsRunning(path)
	if err != nil || running {
		t.Fatalf("missing pidfile probe = (%v, %v), want (false, nil)", running, err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("status probe created its parent directory: %v", err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const stale = "314159"
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	running, err = IsRunning(path)
	if err != nil || running {
		t.Fatalf("unlocked pidfile probe = (%v, %v), want (false, nil)", running, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != stale {
		t.Fatalf("status probe rewrote pidfile to %q, want %q", data, stale)
	}
}

func TestDaemonOperationsPropagateProbeErrors(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "pidfile-is-a-directory")
	if err := os.Mkdir(pidfile, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := Paths{Pidfile: pidfile, Logfile: filepath.Join(t.TempDir(), "akari.log")}
	if running, err := IsRunning(pidfile); err == nil || running {
		t.Fatalf("invalid pidfile probe = (%v, %v), want an error", running, err)
	}
	if err := Start("unused", nil, paths); err == nil {
		t.Fatal("Start swallowed pidfile probe error")
	}
	if err := Stop(paths); err == nil {
		t.Fatal("Stop swallowed pidfile probe error")
	}
	if _, _, err := Status(paths); err == nil {
		t.Fatal("Status swallowed pidfile probe error")
	}
}
