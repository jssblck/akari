package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Paths locates the daemon's pidfile (also the lock) and its log file.
type Paths struct {
	Pidfile string
	Logfile string
}

// DefaultPaths returns the per-user daemon paths under the config directory.
func DefaultPaths() (Paths, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return Paths{}, fmt.Errorf("locate user config dir: %w", err)
	}
	base := filepath.Join(dir, "akari")
	return Paths{
		Pidfile: filepath.Join(base, "akari.pid"),
		Logfile: filepath.Join(base, "akari.log"),
	}, nil
}

// Start launches `self watchArgs...` as a detached background process whose
// output goes to the log file. The child acquires the lock itself; Start waits
// briefly to confirm an instance is holding it.
func Start(self string, watchArgs []string, p Paths) error {
	running, err := IsRunning(p.Pidfile)
	if err != nil {
		return fmt.Errorf("check daemon status: %w", err)
	}
	if running {
		return ErrAlreadyRunning
	}
	if err := os.MkdirAll(filepath.Dir(p.Pidfile), 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(p.Logfile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log %s: %w", p.Logfile, err)
	}
	defer logf.Close()

	proc, err := spawnDetached(self, watchArgs, logf)
	if err != nil {
		return fmt.Errorf("start background process: %w", err)
	}
	childPid := proc.Pid
	// Let the parent exit without reaping or waiting on the child.
	_ = proc.Release()

	// Confirm the child acquired the lock by watching for it to write its own pid.
	// We must not probe the lock ourselves here: competing for it could make the
	// child's own Acquire fail. If the child exits first, it failed to start.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if pid, err := readPid(p.Pidfile); err == nil && pid == childPid {
			return nil
		}
		if !alive(childPid) {
			return fmt.Errorf("background process exited on startup; check %s", p.Logfile)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("background process did not start in time; check %s", p.Logfile)
}

// Stop terminates the running watch process. It verifies a live instance holds
// the lock before reading the pid and signalling, so it cannot kill an unrelated
// process that happens to reuse a stale pid. The terminated process releases its
// own lock on exit (or, on a hard Windows kill, leaves an unlocked file that the
// next start reclaims).
func Stop(p Paths) error {
	running, err := IsRunning(p.Pidfile)
	if err != nil {
		return fmt.Errorf("check daemon status: %w", err)
	}
	if !running {
		return fmt.Errorf("not running")
	}
	pid, err := readPid(p.Pidfile)
	if err != nil {
		return fmt.Errorf("read running daemon pid: %w", err)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := terminate(proc); err != nil {
		return fmt.Errorf("terminate pid %d: %w", pid, err)
	}
	return nil
}

// Status reports whether the watch process is running and its pid. Probe and
// pidfile read failures are returned to the caller.
func Status(p Paths) (running bool, pid int, err error) {
	running, err = IsRunning(p.Pidfile)
	if err != nil || !running {
		return running, 0, err
	}
	pid, err = readPid(p.Pidfile)
	if err != nil {
		return true, 0, fmt.Errorf("read running daemon pid: %w", err)
	}
	return true, pid, nil
}
