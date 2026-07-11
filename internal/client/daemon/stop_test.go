package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestStopConfirmsGracefulShutdownAndLockRelease(t *testing.T) {
	paths := Paths{Pidfile: filepath.Join(t.TempDir(), "akari.pid")}
	lock, err := Acquire(paths.Pidfile)
	if err != nil {
		t.Fatal(err)
	}
	ctx, stopControl, err := lock.ShutdownContext(context.Background())
	if err != nil {
		lock.Release()
		t.Fatal(err)
	}
	cleanupDone := make(chan error, 1)
	go func() {
		<-ctx.Done()
		stopControl()
		cleanupDone <- lock.Release()
	}()

	result, err := Stop(paths, StopOptions{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if result != StoppedGracefully {
		t.Fatalf("result = %v, want graceful", result)
	}
	if err := <-cleanupDone; err != nil {
		t.Fatalf("daemon cleanup: %v", err)
	}

	replacement, err := Acquire(paths.Pidfile)
	if err != nil {
		t.Fatalf("successful stop left lock unavailable: %v", err)
	}
	if err := replacement.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownControlRejectsWrongInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "akari.pid")
	lock, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	ctx, stopControl, err := lock.ShutdownContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stopControl()

	wrong := lock.instance
	wrong.Token = "fedcba9876543210fedcba9876543210"
	requestCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := requestGraceful(requestCtx, path, wrong); err == nil {
		t.Fatal("control accepted the wrong per-run token")
	}
	select {
	case <-ctx.Done():
		t.Fatal("wrong per-run token cancelled the daemon")
	default:
	}
	if err := requestGraceful(requestCtx, path, lock.instance); err != nil {
		t.Fatalf("valid control request: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("valid control request did not cancel the daemon")
	}
}

func TestStopTimesOutWhenDaemonDoesNotReleaseLock(t *testing.T) {
	paths := Paths{Pidfile: filepath.Join(t.TempDir(), "akari.pid")}
	lock, err := Acquire(paths.Pidfile)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	ctx, stopControl, err := lock.ShutdownContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stopControl()

	_, err = Stop(paths, StopOptions{Timeout: 50 * time.Millisecond})
	if !errors.Is(err, ErrShutdownTimeout) {
		t.Fatalf("stop error = %v, want ErrShutdownTimeout", err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("hung daemon did not receive the graceful request")
	}
	if running, probeErr := IsRunning(paths.Pidfile); probeErr != nil || !running {
		t.Fatalf("timed-out daemon lock = (%v, %v), want still held", running, probeErr)
	}
}

func TestStopRejectsStaleUnlockedState(t *testing.T) {
	paths := Paths{Pidfile: filepath.Join(t.TempDir(), "akari.pid")}
	if err := os.WriteFile(paths.Pidfile, []byte(`{"pid":`+strconv.Itoa(os.Getpid())+`,"token":"0123456789abcdef0123456789abcdef"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Stop(paths, StopOptions{Timeout: time.Second, Force: true}); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("stale stop error = %v, want ErrNotRunning", err)
	}
}

func TestStopRefusesForceWhenDaemonIdentityChanges(t *testing.T) {
	paths := Paths{Pidfile: filepath.Join(t.TempDir(), "akari.pid")}
	lock, err := Acquire(paths.Pidfile)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	ctx, stopControl, err := lock.ShutdownContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stopControl()

	rewritten := make(chan error, 1)
	go func() {
		<-ctx.Done()
		rewritten <- os.WriteFile(paths.Pidfile, []byte(`{"pid":2147483646,"token":"fedcba9876543210fedcba9876543210"}`), 0o600)
	}()

	_, err = Stop(paths, StopOptions{Timeout: 100 * time.Millisecond, Force: true})
	if rewriteErr := <-rewritten; rewriteErr != nil {
		t.Fatal(rewriteErr)
	}
	if !errors.Is(err, ErrProcessChanged) {
		t.Fatalf("stop error = %v, want ErrProcessChanged", err)
	}
}

func TestStopForceTerminatesHungDaemonAndConfirmsLockRelease(t *testing.T) {
	if os.Getenv("AKARI_DAEMON_STOP_HELPER") == "1" {
		runHungDaemonHelper()
		return
	}

	paths := Paths{Pidfile: filepath.Join(t.TempDir(), "akari.pid")}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStopForceTerminatesHungDaemonAndConfirmsLockRelease$")
	cmd.Env = append(os.Environ(), "AKARI_DAEMON_STOP_HELPER=1", "AKARI_DAEMON_STOP_PIDFILE="+paths.Pidfile)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-waited:
		case <-time.After(5 * time.Second):
			t.Errorf("helper process was not reaped")
		}
	})
	waitForHelper(t, paths.Pidfile, cmd.Process.Pid)

	result, err := Stop(paths, StopOptions{Timeout: 200 * time.Millisecond, Force: true})
	if err != nil {
		t.Fatalf("forced stop: %v", err)
	}
	if result != StoppedForcefully {
		t.Fatalf("result = %v, want forceful", result)
	}
	if running, probeErr := IsRunning(paths.Pidfile); probeErr != nil || running {
		t.Fatalf("forced daemon lock = (%v, %v), want released", running, probeErr)
	}
}

func runHungDaemonHelper() {
	lock, err := Acquire(os.Getenv("AKARI_DAEMON_STOP_PIDFILE"))
	if err != nil {
		os.Exit(10)
	}
	ctx, _, err := lock.ShutdownContext(context.Background())
	if err != nil {
		os.Exit(11)
	}
	<-ctx.Done()
	select {}
}

func waitForHelper(t *testing.T, pidfile string, pid int) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		inst, err := readInstance(pidfile)
		if err == nil && inst.PID == pid && controlReady(pidfile, inst) {
			return
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("helper pid %d did not acquire its daemon lock", pid)
		}
	}
}
