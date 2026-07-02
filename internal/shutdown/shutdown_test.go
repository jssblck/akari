package shutdown

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

// waitCancelled fails unless ctx is done within the deadline.
func waitCancelled(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context was not cancelled")
	}
}

func TestFirstSignalAcksAndCancels(t *testing.T) {
	ch := make(chan os.Signal, 2)
	acked := make(chan struct{})
	ctx, cancel := watch(ch, func() { close(acked) }, func(int) { t.Error("exit called on first signal") })
	defer cancel()

	ch <- syscall.SIGINT

	select {
	case <-acked:
	case <-time.After(2 * time.Second):
		t.Fatal("ack was not called")
	}
	waitCancelled(t, ctx)
}

// TestAckRunsBeforeCancel guards the ordering the feature depends on: the
// acknowledgement is observable before the context is cancelled, so the log line
// lands before any cancellation-driven teardown begins.
func TestAckRunsBeforeCancel(t *testing.T) {
	ch := make(chan os.Signal, 2)
	cancelledDuringAck := make(chan bool, 1)
	var ctx context.Context
	var cancel func()
	ctx, cancel = watch(ch, func() {
		select {
		case <-ctx.Done():
			cancelledDuringAck <- true
		default:
			cancelledDuringAck <- false
		}
	}, func(int) {})
	defer cancel()

	ch <- syscall.SIGINT
	select {
	case got := <-cancelledDuringAck:
		if got {
			t.Fatal("context was already cancelled when ack ran")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ack was not called")
	}
	waitCancelled(t, ctx)
}

func TestSecondSignalForcesExit(t *testing.T) {
	ch := make(chan os.Signal, 2)
	exited := make(chan int, 1)
	ctx, cancel := watch(ch, func() {}, func(code int) { exited <- code })
	defer cancel()

	ch <- syscall.SIGINT
	waitCancelled(t, ctx)
	ch <- syscall.SIGINT

	select {
	case code := <-exited:
		if code != forcedExitCode {
			t.Fatalf("exit code = %d, want %d", code, forcedExitCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second signal did not force exit")
	}
}

// TestStopBeforeSignal verifies the goroutine exits cleanly when stop is called
// without any signal, and that the ack never fires.
func TestStopBeforeSignal(t *testing.T) {
	ch := make(chan os.Signal, 2)
	var acked bool
	ctx, cancel := watch(ch, func() { acked = true }, func(int) { t.Error("exit called without signal") })

	cancel()
	waitCancelled(t, ctx)
	// Give the goroutine a moment to observe the stop and return.
	time.Sleep(50 * time.Millisecond)
	if acked {
		t.Fatal("ack ran without a signal")
	}
}

// TestStopAfterFirstSignalReleasesWatcher verifies stop releases the goroutine
// that would otherwise wait forever for a second signal: after stop, a late
// signal must not force an exit, because nothing is watching anymore.
func TestStopAfterFirstSignalReleasesWatcher(t *testing.T) {
	ch := make(chan os.Signal, 2)
	ctx, cancel := watch(ch, func() {}, func(int) { t.Error("exit called after stop") })

	ch <- syscall.SIGINT
	waitCancelled(t, ctx)
	cancel()
	// Give the goroutine a moment to observe the stop, then confirm a late
	// signal finds no watcher.
	time.Sleep(50 * time.Millisecond)
	ch <- syscall.SIGINT
	time.Sleep(50 * time.Millisecond)
}
