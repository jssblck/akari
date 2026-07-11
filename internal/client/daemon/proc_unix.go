//go:build !windows

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// spawnDetached starts the watch process in its own session so it survives the
// parent exiting and the controlling terminal closing.
func spawnDetached(self string, args []string) (*os.Process, error) {
	cmd := exec.Command(self, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

// terminate asks the process to shut down cleanly so it can release its lock.
func terminate(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}

// lockFile takes a non-blocking exclusive advisory lock on the open file. flock
// locks attach to the open file description and conflict across descriptions even
// within one process, and the kernel drops them when the process exits.
func lockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func isLockContention(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// alive reports whether a pid names a live process. Signal 0 checks existence;
// EPERM means the process exists but is owned by someone else, which still counts
// as alive.
func alive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
