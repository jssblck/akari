package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestAcquireAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "akari.pid")

	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// A second acquire while the first is held (by this live process) must fail.
	if _, err := Acquire(path); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second acquire error = %v, want ErrAlreadyRunning", err)
	}

	if err := lock.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	// After release the lock is free again.
	lock2, err := Acquire(path)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	lock2.Release()
}

func TestAcquireReclaimsStaleLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "akari.pid")
	// A pidfile naming a process that does not exist is stale and reclaimable.
	deadPID := 0x7ffffffe
	if err := os.WriteFile(path, []byte(strconv.Itoa(deadPID)), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatalf("should reclaim stale lock, got: %v", err)
	}
	defer lock.Release()

	// The reclaimed lock now names this process.
	pid, err := readPid(path)
	if err != nil || pid != os.Getpid() {
		t.Errorf("pidfile pid = %d (err=%v), want %d", pid, err, os.Getpid())
	}
}

func TestIsRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "akari.pid")
	if running, err := IsRunning(path); err != nil || running {
		t.Error("nothing holds the lock yet")
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if running, err := IsRunning(path); err != nil || !running {
		t.Error("the held lock should report running")
	}
	lock.Release()
	if running, err := IsRunning(path); err != nil || running {
		t.Error("released lock should report not running")
	}
}

func TestStatusNotRunning(t *testing.T) {
	paths := Paths{Pidfile: filepath.Join(t.TempDir(), "akari.pid")}
	if running, _, err := Status(paths); err != nil || running {
		t.Error("status should be not-running when no pidfile exists")
	}
}

func TestAliveSelf(t *testing.T) {
	if !alive(os.Getpid()) {
		t.Error("the current process should be reported alive")
	}
	if alive(0x7ffffffe) {
		t.Error("a nonexistent pid should be reported not alive")
	}
}
