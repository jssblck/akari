package upload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	defaultDialTimeout           = 10 * time.Second
	defaultTLSHandshakeTimeout   = 10 * time.Second
	defaultResponseHeaderTimeout = 30 * time.Second
	defaultIdleProgressTimeout   = 60 * time.Second
	defaultAPIRequestTimeout     = 60 * time.Second
)

// errIngestRedirectRefused is returned by CheckRedirect to refuse every redirect.
// req.Clone in Client.do shares GetBody by reference with the original request,
// so a transport-driven redirect replay would read the body directly rather than
// through the progressBody wrapper, losing idle-progress protection on a request
// that (with the total client timeout removed) has no other wall-clock bound.
// Ingest endpoints never redirect, so refusing is the correct, explicit behavior.
var errIngestRedirectRefused = errors.New("upload client: ingest endpoints do not redirect")

// NewHTTPClient builds the production HTTP client used by sync and watch. The
// transport bounds connection setup and response headers, while upload bodies
// use progress deadlines in Client so a large transfer has no wall-clock cap.
// Redirects are refused; see errIngestRedirectRefused.
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   defaultDialTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = defaultTLSHandshakeTimeout
	transport.ResponseHeaderTimeout = defaultResponseHeaderTimeout
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errIngestRedirectRefused
		},
	}
}

// idleProgressError identifies which half of an HTTP exchange stopped moving.
// It is a timeout so callers and the adaptive upload limiter classify it as a
// transient network failure.
type idleProgressError struct {
	phase   string
	timeout time.Duration
}

func (e *idleProgressError) Error() string {
	return fmt.Sprintf("%s made no progress for %s", e.phase, e.timeout)
}

func (e *idleProgressError) Timeout() bool   { return true }
func (e *idleProgressError) Temporary() bool { return true }

// Is reports an idleProgressError as a context.DeadlineExceeded so errors.Is
// classifies it that way everywhere a deadline is checked for, in particular
// the adaptive upload limiter's isLoadShed: a stalled upload is exactly the
// kind of transient, load-sensitive failure the limiter should back off from.
func (e *idleProgressError) Is(target error) bool {
	return target == context.DeadlineExceeded
}

// progressDeadline cancels one request when its current body has moved no bytes
// for the configured window. A live *time.Timer's callback, once the runtime has
// dispatched it, cannot be retracted by Reset, so a Reset landing at the same
// instant as a stalled callback cannot stop it from running. fire (the callback)
// closes that race by re-checking, under mu, how long has actually elapsed since
// lastProgress: if progress arrived before fire acquired the lock, the window has
// not really elapsed and fire re-arms for the remaining time instead of
// cancelling a request that is not actually stalled.
type progressDeadline struct {
	mu           sync.Mutex
	timer        *time.Timer
	timeout      time.Duration
	lastProgress time.Time
	stopped      bool
	cancel       context.CancelCauseFunc
	phase        string
}

func newProgressDeadline(timeout time.Duration, cancel context.CancelCauseFunc, phase string) *progressDeadline {
	if timeout <= 0 {
		return nil
	}
	d := &progressDeadline{
		timeout:      timeout,
		lastProgress: time.Now(),
		cancel:       cancel,
		phase:        phase,
	}
	d.timer = time.AfterFunc(timeout, d.fire)
	return d
}

func (d *progressDeadline) fire() {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	if elapsed := time.Since(d.lastProgress); elapsed < d.timeout {
		// Progress landed before this callback got the lock: the idle window
		// has not genuinely elapsed. Re-arm for what remains of it rather than
		// cancelling a request that is actively moving.
		d.timer.Reset(d.timeout - elapsed)
		d.mu.Unlock()
		return
	}
	d.stopped = true
	d.mu.Unlock()
	d.cancel(&idleProgressError{phase: d.phase, timeout: d.timeout})
}

func (d *progressDeadline) progress(timeout time.Duration) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	d.lastProgress = time.Now()
	d.timeout = timeout
	d.timer.Reset(timeout)
}

func (d *progressDeadline) stop() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.stopped {
		d.stopped = true
		d.timer.Stop()
	}
}

// progressBody refreshes its deadline only when bytes move. For request bodies,
// net/http stops reading when the socket stops accepting writes, so this also
// detects a peer that has stopped receiving without depending on HTTP/1 details.
type progressBody struct {
	body     io.ReadCloser
	ctx      context.Context
	deadline *progressDeadline
	timeout  time.Duration
	onClose  func()
}

func (b *progressBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if n > 0 {
		b.deadline.progress(b.timeout)
	}
	if err != nil {
		b.deadline.stop()
		if n == 0 {
			var idle *idleProgressError
			if cause := context.Cause(b.ctx); errors.As(cause, &idle) {
				return 0, cause
			}
		}
	}
	return n, err
}

func (b *progressBody) Close() error {
	b.deadline.stop()
	err := b.body.Close()
	if b.onClose != nil {
		b.onClose()
	}
	return err
}

// do sends one request with independent idle-progress windows for the request
// and response bodies. The request context remains the authority for explicit
// cancellation; the derived context exists only so an idle timer can interrupt
// a blocked transport operation.
func (c *Client) do(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	req = req.Clone(ctx)

	requestDeadline := newProgressDeadline(c.idleProgressTimeout, cancel, "request body")
	if req.Body != nil {
		req.Body = &progressBody{
			body:     req.Body,
			ctx:      ctx,
			deadline: requestDeadline,
			timeout:  c.idleProgressTimeout,
		}
	} else {
		requestDeadline.stop()
	}

	resp, err := c.http.Do(req)
	requestDeadline.stop()
	if err != nil {
		cause := context.Cause(ctx)
		cancel(nil)
		var idle *idleProgressError
		if errors.As(cause, &idle) {
			return nil, cause
		}
		return nil, err
	}

	responseDeadline := newProgressDeadline(c.idleProgressTimeout, cancel, "response body")
	resp.Body = &progressBody{
		body:     resp.Body,
		ctx:      ctx,
		deadline: responseDeadline,
		timeout:  c.idleProgressTimeout,
		onClose:  func() { cancel(nil) },
	}
	return resp, nil
}
