package upload

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/platinummonkey/go-concurrency-limits/core"
	"github.com/platinummonkey/go-concurrency-limits/limit"
	"github.com/platinummonkey/go-concurrency-limits/limiter"
	"github.com/platinummonkey/go-concurrency-limits/strategy"
)

// Upload concurrency bounds. The body uploader starts at uploadInitialConcurrency and
// the adaptive limiter then walks the live width between uploadMinConcurrency and
// uploadMaxConcurrency from observed round-trip latency: rising latency (a saturated
// network or a server shedding load) shrinks it, steady latency lets it grow. The max
// also caps the upload worker goroutines, so the pool never outruns what the limiter
// would ever hand out.
const (
	uploadInitialConcurrency = 8
	uploadMinConcurrency     = 2
	uploadMaxConcurrency     = 32
)

// uploadLimiter bounds how many tool-body uploads run at once. acquire blocks until a
// slot frees or ctx ends; the returned slot must be released exactly once, carrying the
// upload's outcome, so an adaptive limiter can retune its width from observed latency.
type uploadLimiter interface {
	acquire(ctx context.Context) (uploadSlot, error)
}

// uploadSlot is one held upload permit. release reports the upload's result so the
// limiter samples it correctly: a nil error is a clean round-trip, a load-shed error
// (the server rejected the request or timed out) tells a loss-sensitive limiter to back
// off hard, and any other error is ignored so a client-side fault does not depress the
// latency estimate with an artificially fast failure.
type uploadSlot interface {
	release(err error)
}

// errUploadLimiterRejected is returned when the limiter declines a slot without the
// context being canceled. The blocking limiter only rejects on context cancellation, so
// this is a belt-and-suspenders signal that should not arise in practice.
var errUploadLimiterRejected = errors.New("upload limiter rejected the request")

// adaptiveUploadLimiter wraps the concurrency-limits library: a Gradient2 limit (which
// tracks a long-term vs. short-term RTT gradient to estimate the right concurrency)
// behind a blocking limiter so acquire applies back-pressure instead of failing fast
// when the current limit is reached.
type adaptiveUploadLimiter struct {
	delegate core.Limiter
}

// newAdaptiveUploadLimiter builds the adaptive limiter. The parameters are static and
// valid, so construction does not fail in practice; the error is propagated only so the
// caller can fall back to a fixed limiter rather than panic.
func newAdaptiveUploadLimiter() (uploadLimiter, error) {
	gradient, err := limit.NewGradient2Limit(
		"akari-cas-upload",
		uploadInitialConcurrency,
		uploadMaxConcurrency,
		uploadMinConcurrency,
		func(limit int) int { return 4 }, // queue headroom the library keeps above the limit
		0.2,                              // RTT smoothing factor (library default)
		600,                              // long-window sample count (library default)
		limit.NoopLimitLogger{},
		core.EmptyMetricRegistryInstance,
	)
	if err != nil {
		return nil, err
	}
	delegate, err := limiter.NewDefaultLimiter(
		gradient,
		int64(time.Second),          // minWindowTime
		int64(time.Second),          // maxWindowTime
		int64(100*time.Microsecond), // minRTTThreshold
		100,                         // windowSize: samples before a window is significant
		strategy.NewSimpleStrategy(gradient.EstimatedLimit()),
		limit.NoopLimitLogger{},
		core.EmptyMetricRegistryInstance,
	)
	if err != nil {
		return nil, err
	}
	// timeout 0: block indefinitely (until a release or ctx cancellation) rather than
	// fail an upload just because the limiter is momentarily full.
	blocking := limiter.NewBlockingLimiter(delegate, 0, limit.NoopLimitLogger{})
	return &adaptiveUploadLimiter{delegate: blocking}, nil
}

func (a *adaptiveUploadLimiter) acquire(ctx context.Context) (uploadSlot, error) {
	listener, ok := a.delegate.Acquire(ctx)
	if !ok || listener == nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, errUploadLimiterRejected
	}
	return adaptiveSlot{listener: listener}, nil
}

type adaptiveSlot struct {
	listener core.Listener
}

func (s adaptiveSlot) release(err error) {
	switch {
	case err == nil:
		s.listener.OnSuccess()
	case isLoadShed(err):
		s.listener.OnDropped()
	default:
		s.listener.OnIgnore()
	}
}

// isLoadShed reports whether err signals server-side overload or a timeout, the signals
// a loss-based limiter should react to by shrinking the limit. A client-side cancellation
// or a 4xx other than 429 is the client's own doing and must not be read as the server
// shedding load.
func isLoadShed(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var se *httpStatusError
	if errors.As(err, &se) {
		switch se.code {
		case http.StatusTooManyRequests,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true
		}
	}
	return false
}

// fixedUploadLimiter is a non-adaptive limiter of a fixed width, used as the fallback
// when the adaptive limiter cannot be constructed and as a deterministic stand-in in
// tests. It is a plain counting semaphore: acquire blocks on a buffered channel, release
// frees the slot regardless of outcome.
type fixedUploadLimiter struct {
	slots chan struct{}
}

func newFixedUploadLimiter(width int) uploadLimiter {
	if width <= 0 {
		width = 1
	}
	return &fixedUploadLimiter{slots: make(chan struct{}, width)}
}

func (f *fixedUploadLimiter) acquire(ctx context.Context) (uploadSlot, error) {
	select {
	case f.slots <- struct{}{}:
		return fixedSlot{parent: f}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

type fixedSlot struct {
	parent *fixedUploadLimiter
}

func (s fixedSlot) release(error) { <-s.parent.slots }
