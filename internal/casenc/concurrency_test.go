package casenc

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewLimitedMatchesNew proves the compression-concurrency bound does not change the
// bytes: a bounded encoder produces the identical key, storage content type, and raw
// length as the unbounded one, for both the in-hand and streamed paths. Determinism is
// the CAS contract, so the semaphore must be transparent to the output.
func TestNewLimitedMatchesNew(t *testing.T) {
	withThreshold(t, 16)
	raw := []byte(strings.Repeat("compress me ", 100))

	plainSHA, plainStored, plainCT := New().EncodeBody(raw)
	limSHA, limStored, limCT := NewLimited(2).EncodeBody(raw)
	if plainSHA != limSHA || plainCT != limCT || !bytes.Equal(plainStored, limStored) {
		t.Fatalf("EncodeBody differs under a bound: %s/%s vs %s/%s", plainSHA, plainCT, limSHA, limCT)
	}

	hsSHA, hsCT, hsLen, err := New().HashStream(context.Background(), bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	limHSSHA, limHSCT, limHSLen, err := NewLimited(2).HashStream(context.Background(), bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if hsSHA != limHSSHA || hsCT != limHSCT || hsLen != limHSLen {
		t.Fatalf("HashStream differs under a bound: %s/%s/%d vs %s/%s/%d", hsSHA, hsCT, hsLen, limHSSHA, limHSCT, limHSLen)
	}
	if hsSHA != plainSHA {
		t.Fatalf("streamed key %s != in-hand key %s", hsSHA, plainSHA)
	}
}

// gateReader serves Threshold bytes for HashStream's peek without blocking, then blocks
// the next read (which happens only after the compression permit is acquired) so the
// test can observe how many compressions hold a permit at once. onGate fires once, when
// the post-acquire phase begins; release unblocks it.
type gateReader struct {
	peek    []byte
	peekPos int
	tail    []byte
	tailPos int
	gated   bool
	onGate  func()
	release <-chan struct{}
}

func (r *gateReader) Read(p []byte) (int, error) {
	if r.peekPos < len(r.peek) {
		n := copy(p, r.peek[r.peekPos:])
		r.peekPos += n
		return n, nil
	}
	if !r.gated {
		r.gated = true
		r.onGate()
		<-r.release
	}
	if r.tailPos < len(r.tail) {
		n := copy(p, r.tail[r.tailPos:])
		r.tailPos += n
		return n, nil
	}
	return 0, io.EOF
}

// TestNewLimitedBoundsConcurrentCompression proves the bound is real: with a limit of 2
// and six bodies hashing at once, never more than two are inside the compression at the
// same time. Each gated reader holds its permit (it blocks past the peek), so the other
// four must wait on the semaphore; an unbounded encoder would let all six in.
func TestNewLimitedBoundsConcurrentCompression(t *testing.T) {
	withThreshold(t, 16)
	const limit = 2
	const n = 6
	enc := NewLimited(limit)

	var inFlight, maxInFlight int32
	release := make(chan struct{})
	entered := make(chan struct{}, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := &gateReader{
				peek:    bytes.Repeat([]byte("A"), 16),
				tail:    bytes.Repeat([]byte("B"), 4096),
				release: release,
				onGate: func() {
					cur := atomic.AddInt32(&inFlight, 1)
					for {
						m := atomic.LoadInt32(&maxInFlight)
						if cur <= m || atomic.CompareAndSwapInt32(&maxInFlight, m, cur) {
							break
						}
					}
					entered <- struct{}{}
				},
			}
			if _, _, _, err := enc.HashStream(context.Background(), r); err != nil {
				t.Errorf("hash: %v", err)
			}
		}()
	}

	// Wait until the bound's worth of compressions are inside; the rest must be blocked
	// on the semaphore. Pause to give a broken (unbounded) encoder time to let extras in.
	for i := 0; i < limit; i++ {
		<-entered
	}
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&maxInFlight); got != limit {
		close(release)
		wg.Wait()
		t.Fatalf("peak concurrent compressions = %d, want exactly the bound %d", got, limit)
	}

	close(release)
	wg.Wait()
}
