// Package daemon runs the watch loop as a background process and enforces a
// single running instance per machine. The lock is a real OS advisory file lock
// (flock on unix, LockFileEx on Windows) held for the process lifetime, so it is
// released automatically if the process dies and is immune to pid reuse. The
// file's contents are the holder's pid, kept only as metadata for stop/status.
package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Lock is a held single-instance lock.
type Lock struct {
	f    *os.File
	path string
}

// Acquire takes the lock, returning an error if another live instance holds it.
// The OS releases the lock automatically when this process exits, so there is no
// stale-lock reclaim logic and no TOCTOU window.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("another akari instance is already running")
	}
	// We hold the lock: record our pid (truncate first in case a stale, unlocked
	// file from a hard-killed predecessor remains). A write failure is fatal,
	// since Stop relies on this pid.
	if err := writePid(f); err != nil {
		unlockFile(f)
		f.Close()
		return nil, fmt.Errorf("write pidfile %s: %w", path, err)
	}
	return &Lock{f: f, path: path}, nil
}

func writePid(f *os.File) error {
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid())); err != nil {
		return err
	}
	return f.Sync()
}

// Release drops the OS lock by closing the file handle. It deliberately does not
// remove the pidfile: unlinking after unlocking opens a window in which another
// process acquires the lock and we then delete the path out from under it,
// allowing two holders. A lingering unlocked pidfile is harmless because
// IsRunning probes the live lock, not the file's existence.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlockFile(l.f)
	cerr := l.f.Close()
	l.f = nil
	if err != nil {
		return err
	}
	return cerr
}

// IsRunning reports whether an instance currently holds the lock. It probes by
// attempting to acquire: success (no one holds it) is released immediately. This
// is authoritative regardless of pid reuse, because it tests the live OS lock
// rather than trusting the recorded pid.
func IsRunning(path string) bool {
	l, err := Acquire(path)
	if err != nil {
		return true // the lock is held by a live instance
	}
	// We grabbed it, so nobody was running. Release without deleting a file that
	// a real holder might own: there is no holder, so removing our own is fine.
	l.Release()
	return false
}

func readPid(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}
