package httpapi

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPasswordWorkBoundsActiveAndQueuedCalls(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var active atomic.Int32
	var peak atomic.Int32
	ops := passwordOperations{hash: func(string) (string, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		return "hash", nil
	}}
	w := newPasswordWorkWithOperations(2, 2, time.Second, ops)

	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := w.Hash(context.Background(), "password")
			errs <- err
		}()
	}
	<-started
	<-started
	deadline := time.After(time.Second)
	for len(w.admitted) != cap(w.admitted) {
		select {
		case <-deadline:
			t.Fatalf("admitted calls = %d, want %d", len(w.admitted), cap(w.admitted))
		default:
			runtime.Gosched()
		}
	}
	if _, err := w.Hash(context.Background(), "overflow"); !errors.Is(err, errPasswordWorkUnavailable) {
		t.Fatalf("overflow Hash error = %v, want %v", err, errPasswordWorkUnavailable)
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("admitted Hash error = %v", err)
		}
	}
	if got := peak.Load(); got != 2 {
		t.Fatalf("peak active work = %d, want 2", got)
	}
}

func TestPasswordWorkQueueCancellationReleasesAdmission(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	ops := passwordOperations{hash: func(string) (string, error) {
		started <- struct{}{}
		<-release
		return "hash", nil
	}}
	w := newPasswordWorkWithOperations(1, 1, time.Second, ops)

	firstDone := make(chan error, 1)
	go func() {
		_, err := w.Hash(context.Background(), "first")
		firstDone <- err
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		_, err := w.Hash(ctx, "second")
		secondDone <- err
	}()
	cancel()
	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Hash error = %v, want context.Canceled", err)
	}

	thirdDone := make(chan error, 1)
	go func() {
		_, err := w.Hash(context.Background(), "third")
		thirdDone <- err
	}()
	release <- struct{}{}
	if err := <-firstDone; err != nil {
		t.Fatalf("first Hash error = %v", err)
	}
	<-started
	release <- struct{}{}
	if err := <-thirdDone; err != nil {
		t.Fatalf("third Hash error = %v", err)
	}
}

func TestPasswordWorkQueueTimeoutReleasesAdmission(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{}, 2)
	ops := passwordOperations{verify: func(string, string) (bool, error) {
		started <- struct{}{}
		<-release
		return true, nil
	}}
	w := newPasswordWorkWithOperations(1, 1, 20*time.Millisecond, ops)

	firstDone := make(chan error, 1)
	go func() {
		_, err := w.Verify(context.Background(), "first", "hash")
		firstDone <- err
	}()
	<-started
	if _, err := w.Verify(context.Background(), "queued", "hash"); !errors.Is(err, errPasswordWorkUnavailable) {
		t.Fatalf("timed-out Verify error = %v, want %v", err, errPasswordWorkUnavailable)
	}

	thirdDone := make(chan error, 1)
	go func() {
		_, err := w.Verify(context.Background(), "third", "hash")
		thirdDone <- err
	}()
	release <- struct{}{}
	if err := <-firstDone; err != nil {
		t.Fatalf("first Verify error = %v", err)
	}
	<-started
	release <- struct{}{}
	if err := <-thirdDone; err != nil {
		t.Fatalf("third Verify error = %v", err)
	}
}
