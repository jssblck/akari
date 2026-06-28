package daemon

import (
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
	if _, err := Acquire(path); err == nil {
		t.Fatal("second acquire should fail while the lock is held")
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
	pid, ok := readPid(path)
	if !ok || pid != os.Getpid() {
		t.Errorf("pidfile pid = %d (ok=%v), want %d", pid, ok, os.Getpid())
	}
}

func TestIsRunning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "akari.pid")
	if IsRunning(path) {
		t.Error("nothing holds the lock yet")
	}
	lock, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if !IsRunning(path) {
		t.Error("the held lock should report running")
	}
	lock.Release()
	if IsRunning(path) {
		t.Error("released lock should report not running")
	}
}

func TestStatusNotRunning(t *testing.T) {
	paths := Paths{Pidfile: filepath.Join(t.TempDir(), "akari.pid")}
	if running, _ := Status(paths); running {
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
