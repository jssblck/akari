// Package shutdown turns the first interrupt into a graceful stop and the second
// into an immediate exit.
//
// Both akari binaries want the same behavior on Ctrl-C: acknowledge the request
// at once, let in-flight work wind down to a clean stopping point, then exit; and
// if the operator interrupts again, stop waiting and quit now. The context this
// package returns is cancelled on the first signal. It is meant for deciding
// whether to start new work, not for tearing down work already underway, so a
// single Ctrl-C lets the current unit finish: detach long-running calls with
// context.WithoutCancel and gate the loop on the returned context.
package shutdown

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// forcedExitCode is the conventional status for a process killed by Ctrl-C
// (128 + SIGINT). The second interrupt exits with it.
const forcedExitCode = 130

// Notify installs an interrupt handler and returns a context cancelled on the
// first SIGINT or SIGTERM. ack runs once, just before the cancel, so the caller
// can log that shutdown has begun the instant the signal lands. A second signal
// exits the process with status 130 instead of waiting for graceful cleanup.
//
// The returned stop unregisters the handler and cancels the context; defer it.
func Notify(ack func()) (ctx context.Context, stop func()) {
	// Buffer two: the graceful signal and the forceful one can both be queued
	// before the goroutine drains them.
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	ctx, cancel := watch(ch, ack, os.Exit)
	return ctx, func() {
		signal.Stop(ch)
		cancel()
	}
}

// watch implements the graceful-then-forceful policy over an arbitrary signal
// channel and exit function. Keeping it separate from Notify lets tests drive the
// channel and observe the forced exit without sending real signals or killing the
// test process.
func watch(ch <-chan os.Signal, ack func(), exit func(int)) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		select {
		case <-ch:
			ack()
			cancel()
		case <-ctx.Done():
			return // stop() called before any signal; nothing to acknowledge
		}
		// Graceful shutdown is underway. The next signal means the operator is
		// done waiting, so abandon the clean path and exit now.
		<-ch
		exit(forcedExitCode)
	}()
	return ctx, cancel
}
