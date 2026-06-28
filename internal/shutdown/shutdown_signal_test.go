//go:build unix

package shutdown

import (
	"syscall"
	"testing"
	"time"
)

// TestNotifyRealSignal exercises the full wiring: a real SIGINT delivered to the
// process must run the ack and cancel the context. It sends only one signal, so
// the forced-exit path never fires and the test process survives. Unix-only,
// since Windows offers no portable way to deliver SIGINT to oneself.
func TestNotifyRealSignal(t *testing.T) {
	acked := make(chan struct{})
	ctx, stop := Notify(func() { close(acked) })
	defer stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatalf("send SIGINT: %v", err)
	}

	select {
	case <-acked:
	case <-time.After(2 * time.Second):
		t.Fatal("ack was not called after real SIGINT")
	}
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("context was not cancelled after real SIGINT")
	}
}
