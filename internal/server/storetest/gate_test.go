package storetest

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStoreGateBoundsConcurrency is the load-bearing check for the cross-process
// design: it proves the flock-backed semaphore actually bounds concurrent holders
// on this platform, since that is the whole reason the suite can drop its `-p`
// pin. It runs many more goroutines than there are slots and asserts the gate
// never lets more than n hold at once.
//
// Goroutines in one process contend here through separate flock handles on the
// same slot files, exactly as separate `go test` binaries would, so a green run
// exercises the same locking path that bounds the real suite across processes.
func TestStoreGateBoundsConcurrency(t *testing.T) {
	t.Parallel()

	const (
		slots      = 4
		goroutines = 40
	)
	g, err := newStoreGate(t.TempDir(), slots)
	if err != nil {
		t.Fatalf("new gate: %v", err)
	}

	var live, maxLive atomic.Int64
	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lock := g.acquire()
			cur := live.Add(1)
			for { // record the high-water mark of concurrent holders
				m := maxLive.Load()
				if cur <= m || maxLive.CompareAndSwap(m, cur) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond) // hold long enough to overlap
			live.Add(-1)
			if err := lock.Unlock(); err != nil {
				t.Errorf("unlock slot: %v", err)
			}
		}()
	}
	wg.Wait()

	switch got := maxLive.Load(); {
	case got == 0:
		t.Fatal("no holders observed; gate never admitted a goroutine")
	case got > slots:
		t.Fatalf("gate admitted %d concurrent holders, want at most %d", got, slots)
	}
}

// TestStoreGateReleaseFreesSlot checks that a released slot is reusable, so the
// gate does not slowly bleed capacity as tests come and go: with a single slot,
// serial acquire/release must succeed every time rather than deadlock on the
// second acquire.
func TestStoreGateReleaseFreesSlot(t *testing.T) {
	t.Parallel()

	g, err := newStoreGate(t.TempDir(), 1)
	if err != nil {
		t.Fatalf("new gate: %v", err)
	}
	for i := range 5 {
		lock := g.acquire()
		if err := lock.Unlock(); err != nil {
			t.Fatalf("unlock on iteration %d: %v", i, err)
		}
	}
}
