package upload

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// TestAdaptiveUploadLimiterAcquireRelease proves the adaptive limiter constructs from
// its static config, hands out a slot, and accepts each outcome without panicking. It
// also confirms acquire honors a canceled context rather than blocking forever.
func TestAdaptiveUploadLimiterAcquireRelease(t *testing.T) {
	lim, err := newAdaptiveUploadLimiter()
	if err != nil {
		t.Fatalf("construct adaptive limiter: %v", err)
	}

	for _, outcome := range []error{nil, &httpStatusError{code: http.StatusServiceUnavailable}, errors.New("some other error")} {
		slot, err := lim.acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		slot.release(outcome) // must not panic regardless of the outcome class
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lim.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire with canceled ctx = %v, want context.Canceled", err)
	}
}

// TestIsLoadShed checks the classification that drives the adaptive back-off: server
// overload codes and timeouts count as load-shed, a client cancellation or an ordinary
// 4xx does not.
func TestIsLoadShed(t *testing.T) {
	shed := []error{
		&httpStatusError{code: http.StatusTooManyRequests},
		&httpStatusError{code: http.StatusServiceUnavailable},
		&httpStatusError{code: http.StatusInternalServerError},
		&httpStatusError{code: http.StatusBadGateway},
		&httpStatusError{code: http.StatusGatewayTimeout},
		context.DeadlineExceeded,
		&idleProgressError{phase: "request body", timeout: time.Second},
	}
	for _, err := range shed {
		if !isLoadShed(err) {
			t.Errorf("isLoadShed(%v) = false, want true", err)
		}
	}
	notShed := []error{
		nil,
		&httpStatusError{code: http.StatusBadRequest},
		&httpStatusError{code: http.StatusNotFound},
		context.Canceled,
		errors.New("plain error"),
	}
	for _, err := range notShed {
		if isLoadShed(err) {
			t.Errorf("isLoadShed(%v) = true, want false", err)
		}
	}
}

// TestIsLoadShedClassifiesIdleProgressStall is a targeted regression test for the
// idleProgressError/isLoadShed gap: idleProgressError's doc comment has always
// claimed the limiter treats a stalled upload as transient, but until
// idleProgressError implemented Is, errors.Is(err, context.DeadlineExceeded)
// never matched it and the adaptive limiter never backed off on a stall.
func TestIsLoadShedClassifiesIdleProgressStall(t *testing.T) {
	err := &idleProgressError{phase: "response body", timeout: 30 * time.Second}
	if !isLoadShed(err) {
		t.Fatalf("isLoadShed(%v) = false, want true: idle-progress stalls must be treated as load-shed so the adaptive limiter backs off", err)
	}
}

// TestFixedUploadLimiterBounds proves the fallback/test limiter is a strict counting
// semaphore: it grants up to its width, blocks the next acquire until a slot frees, and
// honors a canceled context while blocked.
func TestFixedUploadLimiterBounds(t *testing.T) {
	lim := newFixedUploadLimiter(2)
	s1, err := lim.acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lim.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The third acquire must block until a slot frees.
	got := make(chan error, 1)
	go func() {
		_, err := lim.acquire(context.Background())
		got <- err
	}()
	select {
	case <-got:
		t.Fatal("third acquire returned while both slots were held")
	case <-time.After(30 * time.Millisecond):
	}
	s1.release(nil) // free a slot; the blocked acquire now proceeds
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("unblocked acquire: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("third acquire did not proceed after a release")
	}

	// A canceled context aborts a blocked acquire (both slots are held again).
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := lim.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire with canceled ctx = %v, want context.Canceled", err)
	}
}
