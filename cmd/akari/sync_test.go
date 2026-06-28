package main

import (
	"context"
	"testing"
	"time"
)

// TestSyncDeadlineCancelsAfterLimit verifies the time limit behaves like a
// self-inflicted graceful shutdown: the context the sync loop gates on cancels on
// its own once the limit elapses, with the deadline as the cause.
func TestSyncDeadlineCancelsAfterLimit(t *testing.T) {
	deadline, cancel := syncDeadline(context.Background(), 20*time.Millisecond)
	defer cancel()

	select {
	case <-deadline.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("deadline context was not cancelled after the limit elapsed")
	}
	if got := deadline.Err(); got != context.DeadlineExceeded {
		t.Fatalf("deadline.Err() = %v, want %v", got, context.DeadlineExceeded)
	}
}

// TestSyncDeadlineZeroMeansNoLimit guards the documented infinite case: a
// non-positive limit must leave the context live so the loop runs until the work
// is done rather than stopping itself.
func TestSyncDeadlineZeroMeansNoLimit(t *testing.T) {
	for _, limit := range []time.Duration{0, -time.Second} {
		deadline, cancel := syncDeadline(context.Background(), limit)
		if _, hasDeadline := deadline.Deadline(); hasDeadline {
			cancel()
			t.Fatalf("limit %v: context has a deadline, want none", limit)
		}
		select {
		case <-deadline.Done():
			cancel()
			t.Fatalf("limit %v: context cancelled itself, want live until cancel", limit)
		case <-time.After(50 * time.Millisecond):
		}
		cancel()
	}
}

// TestSyncDeadlinePropagatesParentCancel confirms a Ctrl-C on the parent shutdown
// context still stops the loop even when a finite time limit is in force, so the
// two stop conditions compose instead of one masking the other.
func TestSyncDeadlinePropagatesParentCancel(t *testing.T) {
	parent, parentCancel := context.WithCancel(context.Background())
	deadline, cancel := syncDeadline(parent, time.Hour)
	defer cancel()

	parentCancel()
	select {
	case <-deadline.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("deadline context did not cancel when parent was cancelled")
	}
}
