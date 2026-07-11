// Package daemon runs the watch loop as a background process and enforces a
// single running instance per machine. The lock is a real OS advisory file lock
// (flock on unix, LockFileEx on Windows) held for the process lifetime, so it is
// released automatically if the process dies and is immune to pid reuse. The
// file's contents are the holder's pid and a random per-run token, kept as
// metadata for stop/status and rechecked before forceful termination.
package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrAlreadyRunning reports lock contention from another daemon instance.
var ErrAlreadyRunning = errors.New("another akari instance is already running")

// Lock is a held single-instance lock.
type Lock struct {
	f        *os.File
	path     string
	instance instance
}

type instance struct {
	PID   int    `json:"pid"`
	Token string `json:"token"`
}

// Acquire takes the lock. It returns ErrAlreadyRunning when another live
// instance holds it; other filesystem and lock failures preserve their causes.
// The OS releases the lock automatically when this process exits, so there is no
// stale-lock reclaim logic and no TOCTOU window.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create pidfile directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open pidfile %s: %w", path, err)
	}
	if err := lockFile(f); err != nil {
		lockErr := fmt.Errorf("lock pidfile %s: %w", path, err)
		if isLockContention(err) {
			lockErr = fmt.Errorf("%w: %s", ErrAlreadyRunning, path)
		}
		if closeErr := f.Close(); closeErr != nil {
			lockErr = errors.Join(lockErr, fmt.Errorf("close pidfile %s: %w", path, closeErr))
		}
		return nil, lockErr
	}
	// We hold the lock: record our pid (truncate first in case a stale, unlocked
	// file from a hard-killed predecessor remains). A write failure is fatal,
	// since Stop relies on this identity.
	inst, err := newInstance()
	if err != nil {
		return nil, releaseAfterAcquireFailure(f, path, fmt.Errorf("create daemon identity: %w", err))
	}
	if err := writeInstance(f, inst); err != nil {
		writeErr := fmt.Errorf("write pidfile %s: %w", path, err)
		return nil, releaseAfterAcquireFailure(f, path, writeErr)
	}
	return &Lock{f: f, path: path, instance: inst}, nil
}

func newInstance() (instance, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return instance{}, err
	}
	return instance{PID: os.Getpid(), Token: hex.EncodeToString(token[:])}, nil
}

func releaseAfterAcquireFailure(f *os.File, path string, cause error) error {
	if unlockErr := unlockFile(f); unlockErr != nil {
		cause = errors.Join(cause, fmt.Errorf("unlock pidfile %s: %w", path, unlockErr))
	}
	if closeErr := f.Close(); closeErr != nil {
		cause = errors.Join(cause, fmt.Errorf("close pidfile %s: %w", path, closeErr))
	}
	return cause
}

func writeInstance(f *os.File, inst instance) error {
	data, err := json.Marshal(inst)
	if err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
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
		err = fmt.Errorf("unlock pidfile %s: %w", l.path, err)
	}
	if cerr != nil {
		cerr = fmt.Errorf("close pidfile %s: %w", l.path, cerr)
	}
	if err != nil {
		return errors.Join(err, cerr)
	}
	return cerr
}

// IsRunning reports whether an instance currently holds the lock without
// creating the pidfile or changing its contents. Only lock contention means
// running; permission, filesystem, lock, and close failures remain visible.
func IsRunning(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open pidfile %s: %w", path, err)
	}
	if err := lockFile(f); err != nil {
		if isLockContention(err) {
			if closeErr := f.Close(); closeErr != nil {
				return false, fmt.Errorf("close pidfile %s: %w", path, closeErr)
			}
			return true, nil
		}
		lockErr := fmt.Errorf("probe pidfile lock %s: %w", path, err)
		if closeErr := f.Close(); closeErr != nil {
			lockErr = errors.Join(lockErr, fmt.Errorf("close pidfile %s: %w", path, closeErr))
		}
		return false, lockErr
	}
	unlockErr := unlockFile(f)
	closeErr := f.Close()
	if unlockErr != nil {
		err := fmt.Errorf("unlock pidfile %s: %w", path, unlockErr)
		if closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close pidfile %s: %w", path, closeErr))
		}
		return false, err
	}
	if closeErr != nil {
		return false, fmt.Errorf("close pidfile %s: %w", path, closeErr)
	}
	return false, nil
}

func readPid(path string) (int, error) {
	inst, err := readInstance(path)
	if err != nil {
		return 0, err
	}
	return inst.PID, nil
}

func readInstance(path string) (instance, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return instance{}, fmt.Errorf("read pidfile %s: %w", path, err)
	}
	var inst instance
	if err := json.Unmarshal(data, &inst); err != nil {
		return instance{}, fmt.Errorf("parse pidfile %s: %w", path, err)
	}
	if inst.PID <= 0 || len(inst.Token) != 32 {
		return instance{}, fmt.Errorf("pidfile %s does not contain a valid daemon identity", path)
	}
	if _, err := hex.DecodeString(inst.Token); err != nil {
		return instance{}, fmt.Errorf("pidfile %s does not contain a valid daemon identity", path)
	}
	return inst, nil
}
