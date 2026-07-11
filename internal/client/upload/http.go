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

// NewHTTPClient builds the production HTTP client used by sync and watch. The
// transport bounds connection setup and response headers, while upload bodies
// use progress deadlines in Client so a large transfer has no wall-clock cap.
func NewHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   defaultDialTimeout,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = defaultTLSHandshakeTimeout
	transport.ResponseHeaderTimeout = defaultResponseHeaderTimeout
	return &http.Client{Transport: transport}
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

// progressDeadline cancels one request when its current body has moved no bytes
// for the configured window. Reset, stop, and expiry serialize on mu so exactly
// one terminal state wins.
type progressDeadline struct {
	mu      sync.Mutex
	timer   *time.Timer
	stopped bool
}

func newProgressDeadline(timeout time.Duration, cancel context.CancelCauseFunc, phase string) *progressDeadline {
	if timeout <= 0 {
		return nil
	}
	d := &progressDeadline{}
	d.timer = time.AfterFunc(timeout, func() {
		d.mu.Lock()
		if d.stopped {
			d.mu.Unlock()
			return
		}
		d.stopped = true
		d.mu.Unlock()
		cancel(&idleProgressError{phase: phase, timeout: timeout})
	})
	return d
}

func (d *progressDeadline) progress(timeout time.Duration) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.stopped {
		d.timer.Reset(timeout)
	}
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
