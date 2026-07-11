package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// DefaultStopTimeout bounds both the graceful wait and, when requested, the
	// confirmation wait after forceful termination.
	DefaultStopTimeout = 10 * time.Second
	stopPollInterval   = 25 * time.Millisecond
)

var (
	ErrNotRunning      = errors.New("not running")
	ErrShutdownTimeout = errors.New("graceful shutdown timed out")
	ErrForceTimeout    = errors.New("forced shutdown timed out")
	ErrProcessChanged  = errors.New("daemon identity changed during shutdown")
)

// StopOptions controls the bounded stop sequence.
type StopOptions struct {
	Timeout time.Duration
	Force   bool
}

// StopResult reports how a confirmed stop completed.
type StopResult int

const (
	StoppedGracefully StopResult = iota
	StoppedForcefully
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
		if inst, err := readInstance(p.Pidfile); err == nil && inst.PID == childPid && controlReady(p.Pidfile, inst) {
			return nil
		}
		if !alive(childPid) {
			return fmt.Errorf("background process exited on startup; check %s", p.Logfile)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("background process did not start in time; check %s", p.Logfile)
}

// Stop requests graceful shutdown and does not succeed until the daemon lock is
// free. Force permits escalation after a failed request or timeout; the recorded
// per-run identity is revalidated immediately before terminating the process.
func Stop(p Paths, opts StopOptions) (StopResult, error) {
	if opts.Timeout == 0 {
		opts.Timeout = DefaultStopTimeout
	}
	if opts.Timeout < 0 {
		return StoppedGracefully, fmt.Errorf("stop timeout must be positive")
	}
	running, err := IsRunning(p.Pidfile)
	if err != nil {
		return StoppedGracefully, fmt.Errorf("check daemon status: %w", err)
	}
	if !running {
		return StoppedGracefully, ErrNotRunning
	}
	inst, err := readInstance(p.Pidfile)
	if err != nil {
		if released, probeErr := daemonLockReleased(p.Pidfile); probeErr == nil && released {
			return StoppedGracefully, nil
		}
		return StoppedGracefully, fmt.Errorf("read running daemon identity: %w", err)
	}
	proc, err := os.FindProcess(inst.PID)
	if err != nil {
		if released, probeErr := daemonLockReleased(p.Pidfile); probeErr == nil && released {
			return StoppedGracefully, nil
		}
		return StoppedGracefully, fmt.Errorf("open daemon process %d: %w", inst.PID, err)
	}
	defer proc.Release()

	gracefulCtx, cancel := context.WithTimeout(context.Background(), opts.Timeout)
	requestErr := requestGraceful(gracefulCtx, p.Pidfile, inst)
	if requestErr == nil {
		if err := waitForRelease(gracefulCtx, p.Pidfile); err == nil {
			cancel()
			return StoppedGracefully, nil
		} else if !errors.Is(err, context.DeadlineExceeded) {
			cancel()
			return StoppedGracefully, err
		}
	}
	cancel()

	stillRunning, err := sameInstanceRunning(p.Pidfile, inst)
	if err != nil {
		return StoppedGracefully, err
	}
	if !stillRunning {
		return StoppedGracefully, nil
	}
	if !opts.Force {
		if requestErr != nil {
			return StoppedGracefully, fmt.Errorf("request graceful shutdown: %w", requestErr)
		}
		return StoppedGracefully, fmt.Errorf("%w after %s; daemon is still running (use --force to terminate it)", ErrShutdownTimeout, opts.Timeout)
	}

	if err := forceTerminate(proc); err != nil {
		return StoppedGracefully, fmt.Errorf("force daemon process %d: %w", inst.PID, err)
	}
	forceCtx, forceCancel := context.WithTimeout(context.Background(), opts.Timeout)
	defer forceCancel()
	if err := waitForRelease(forceCtx, p.Pidfile); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return StoppedGracefully, fmt.Errorf("%w after %s", ErrForceTimeout, opts.Timeout)
		}
		return StoppedGracefully, err
	}
	return StoppedForcefully, nil
}

func daemonLockReleased(pidfile string) (bool, error) {
	running, err := IsRunning(pidfile)
	return !running, err
}

func sameInstanceRunning(pidfile string, expected instance) (bool, error) {
	running, err := IsRunning(pidfile)
	if err != nil {
		return false, fmt.Errorf("recheck daemon lock: %w", err)
	}
	if !running {
		return false, nil
	}
	current, err := readInstance(pidfile)
	if err != nil {
		return false, fmt.Errorf("re-read daemon identity: %w", err)
	}
	if current != expected {
		return false, fmt.Errorf("%w: expected pid %d, found pid %d", ErrProcessChanged, expected.PID, current.PID)
	}
	return true, nil
}

func waitForRelease(ctx context.Context, pidfile string) error {
	ticker := time.NewTicker(stopPollInterval)
	defer ticker.Stop()
	for {
		running, err := IsRunning(pidfile)
		if err != nil {
			return fmt.Errorf("confirm daemon lock release: %w", err)
		}
		if !running {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
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
