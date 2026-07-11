//go:build windows

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// Windows process creation flags for a detached, window-less background process.
const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
	createNoWindow        = 0x08000000
)

// spawnDetached starts the watch process detached from the console so it keeps
// running with no visible window after the launching shell exits.
func spawnDetached(self string, args []string) (*os.Process, error) {
	cmd := exec.Command(self, args...)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | detachedProcess | createNoWindow,
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Process, nil
}

// lockRegion locks a single byte at a high sentinel offset well past any real
// pidfile content. On Windows an exclusive LockFileEx range is unreadable by
// other handles, so locking the data bytes would stop Stop/Status from reading
// the pid; locking out beyond EOF keeps mutual exclusion while leaving the pid
// readable.
func lockRegion() *windows.Overlapped {
	return &windows.Overlapped{OffsetHigh: 0x7fffffff}
}

// lockFile takes a non-blocking exclusive lock on the open file via LockFileEx.
// The lock conflicts across handles even within one process, and Windows
// releases it when the owning handle closes (including on process exit).
func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, lockRegion())
}

func isLockContention(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, lockRegion())
}

// forceTerminate is used only after the named-event graceful path has failed
// and the caller has explicitly permitted escalation.
func forceTerminate(p *os.Process) error {
	return p.Kill()
}

// alive reports whether a pid names a live process by asking tasklist. Go's
// os.FindProcess always succeeds on Windows, so it cannot be used for liveness.
func alive(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH", "/FO", "CSV").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "\""+strconv.Itoa(pid)+"\"")
}
