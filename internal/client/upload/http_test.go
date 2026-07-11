package upload

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestNewHTTPClientBoundsSetupWithoutTotalTimeout(t *testing.T) {
	c := NewHTTPClient()
	if c.Timeout != 0 {
		t.Fatalf("client timeout = %s, want no total timeout", c.Timeout)
	}
	transport, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", c.Transport)
	}
	if transport.DialContext == nil {
		t.Fatal("dialer is not configured")
	}
	if transport.TLSHandshakeTimeout != defaultTLSHandshakeTimeout {
		t.Fatalf("TLS handshake timeout = %s, want %s", transport.TLSHandshakeTimeout, defaultTLSHandshakeTimeout)
	}
	if transport.ResponseHeaderTimeout != defaultResponseHeaderTimeout {
		t.Fatalf("response header timeout = %s, want %s", transport.ResponseHeaderTimeout, defaultResponseHeaderTimeout)
	}
	if c.CheckRedirect == nil {
		t.Fatal("CheckRedirect is not configured, want redirects refused")
	}
}

// TestNewHTTPClientRefusesRedirects guards the assumption behind progressBody:
// req.Clone shares GetBody by reference, so a transport-driven redirect replay
// would read a request body directly rather than through progressBody, losing
// idle-progress protection on a request that has no other wall-clock timeout.
// Ingest endpoints never redirect, so the client must refuse to follow one.
func TestNewHTTPClientRefusesRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "should not be reached")
	}))
	t.Cleanup(target.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	c := NewHTTPClient()
	resp, err := c.Get(srv.URL)
	if err == nil {
		resp.Body.Close()
		t.Fatal("GET through a redirecting server succeeded, want the redirect refused")
	}
	if !errors.Is(err, errIngestRedirectRefused) {
		t.Fatalf("error = %v, want errIngestRedirectRefused", err)
	}
}

type pacedReader struct {
	data  []byte
	chunk int
	delay time.Duration
}

func (r *pacedReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	timer := time.NewTimer(r.delay)
	defer timer.Stop()
	<-timer.C
	n := min(len(r.data), min(r.chunk, len(p)))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestSlowProgressingUploadOutlivesFormerTotalTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)

	const formerTotal = 100 * time.Millisecond
	c := New(srv.Client(), srv.URL, "token")
	c.idleProgressTimeout = 250 * time.Millisecond
	body := &pacedReader{data: bytes.Repeat([]byte("x"), 6), chunk: 1, delay: 40 * time.Millisecond}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, srv.URL, body)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	resp, err := c.do(req)
	if err != nil {
		t.Fatalf("progressing upload: %v", err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("read response: %v", err)
	}
	if elapsed := time.Since(started); elapsed <= formerTotal {
		t.Fatalf("request finished in %s, test did not outlive former %s total timeout", elapsed, formerTotal)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestStalledRequestBodyHitsIdleProgressTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	releaseServer := make(chan struct{})
	t.Cleanup(func() { close(releaseServer) })
	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" {
				close(accepted)
				<-releaseServer
				return
			}
		}
	}()

	c := New(NewHTTPClient(), "http://"+listener.Addr().String(), "token")
	c.idleProgressTimeout = 100 * time.Millisecond
	body := io.LimitReader(zeroReader{}, 64<<20)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPut, c.baseURL, body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = 64 << 20
	errCh := make(chan error, 1)
	go func() {
		_, err := c.do(req)
		errCh <- err
	}()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not accept request")
	}
	select {
	case err := <-errCh:
		assertIdlePhase(t, err, "request body")
	case <-time.After(2 * time.Second):
		t.Fatal("stalled upload did not time out")
	}
}

func TestStalledResponseBodyHitsIdleProgressTimeout(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	c := New(srv.Client(), srv.URL, "token")
	c.idleProgressTimeout = 100 * time.Millisecond
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started
	_, err = io.ReadAll(resp.Body)
	assertIdlePhase(t, err, "response body")
}

func TestExplicitCancellationWinsOverIdleProgressTimeout(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	c := New(srv.Client(), srv.URL, "token")
	c.idleProgressTimeout = time.Second
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-started
	cancel()
	_, err = io.ReadAll(resp.Body)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("read error = %v, want context cancellation", err)
	}
}

func TestOrdinaryAPIRequestsStayFastAndKeepTotalTimeout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /fast", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /slow", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := New(srv.Client(), srv.URL, "token")
	c.apiRequestTimeout = 100 * time.Millisecond
	c.idleProgressTimeout = time.Second
	var out map[string]bool
	if err := c.doJSON(context.Background(), http.MethodPost, "/fast", map[string]string{"name": "Ada"}, &out); err != nil {
		t.Fatalf("fast API request: %v", err)
	}
	if !out["ok"] {
		t.Fatalf("fast API response = %#v", out)
	}
	err := c.doJSON(context.Background(), http.MethodPost, "/slow", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("slow API error = %v, want total deadline", err)
	}
}

func TestRetryResumesAfterCommittedChunkResponseStalls(t *testing.T) {
	oldSettle := settleWindow
	settleWindow = 0
	t.Cleanup(func() { settleWindow = oldSettle })

	var mu sync.Mutex
	var stored []byte
	chunkCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/ingest/session", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		body := append([]byte(nil), stored...)
		mu.Unlock()
		sum := sha256.Sum256(body)
		writeJSON(w, map[string]any{
			"session_id":    1,
			"stored_bytes":  len(body),
			"prefix_sha256": hex.EncodeToString(sum[:]),
		})
	})
	mux.HandleFunc("POST /api/v1/ingest/session/1/chunk", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		mu.Lock()
		chunkCalls++
		stored = append(stored, body...)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := New(srv.Client(), srv.URL, "token")
	c.idleProgressTimeout = 100 * time.Millisecond
	path := tempFile(t, "first\nsecond\n")
	if _, err := c.SyncFile(context.Background(), target(path)); err == nil {
		t.Fatal("first sync succeeded despite stalled chunk response")
	} else {
		var idle *idleProgressError
		if !errors.As(err, &idle) {
			t.Fatalf("first sync error = %v, want idle timeout", err)
		}
	}
	out, err := c.SyncFile(context.Background(), target(path))
	if err != nil {
		t.Fatalf("resume sync: %v", err)
	}
	mu.Lock()
	gotStored := string(stored)
	gotCalls := chunkCalls
	mu.Unlock()
	if gotStored != "first\nsecond\n" {
		t.Fatalf("stored bytes = %q, want one copy of transcript", gotStored)
	}
	if gotCalls != 1 {
		t.Fatalf("chunk calls = %d, want committed chunk not to be resent", gotCalls)
	}
	if out.StoredBytes != int64(len(gotStored)) {
		t.Fatalf("resumed cursor = %d, want %d", out.StoredBytes, len(gotStored))
	}
}

// TestProgressDeadlineNoSpuriousCancelWhileProgressContinues is a regression test
// for the Reset race in progressDeadline: once the runtime has dispatched a
// *time.Timer's callback, Reset cannot retract it, so a naive stopped bool cannot
// tell "reset before expiry" apart from "reset racing an already-dispatched
// callback". Here progress lands just shy of the window's expiry, over and over,
// so each call's Reset repeatedly races the timer's own dispatch; a correct
// implementation must never treat steady progress like this as a stall.
func TestProgressDeadlineNoSpuriousCancelWhileProgressContinues(t *testing.T) {
	const window = 60 * time.Millisecond
	const margin = 8 * time.Millisecond
	const iterations = 15

	var mu sync.Mutex
	var cancelErr error
	cancel := context.CancelCauseFunc(func(cause error) {
		mu.Lock()
		defer mu.Unlock()
		if cancelErr == nil {
			cancelErr = cause
		}
	})

	d := newProgressDeadline(window, cancel, "test")
	defer d.stop()

	for i := 0; i < iterations; i++ {
		time.Sleep(window - margin)
		d.progress(window)
	}

	mu.Lock()
	defer mu.Unlock()
	if cancelErr != nil {
		t.Fatalf("progress landed every %s against a %s window, but the deadline still fired: %v", window-margin, window, cancelErr)
	}
}

// TestProgressDeadlineFiresWhenProgressStops complements the no-spurious-cancel
// test above: once progress genuinely stops arriving, the deadline must still
// fire promptly with an *idleProgressError for the configured phase.
func TestProgressDeadlineFiresWhenProgressStops(t *testing.T) {
	const window = 30 * time.Millisecond

	done := make(chan error, 1)
	cancel := context.CancelCauseFunc(func(cause error) { done <- cause })

	d := newProgressDeadline(window, cancel, "test phase")
	defer d.stop()

	d.progress(window)
	time.Sleep(window / 2)
	d.progress(window) // one more heartbeat, then progress genuinely stops

	select {
	case err := <-done:
		var idle *idleProgressError
		if !errors.As(err, &idle) {
			t.Fatalf("cancel cause = %v, want *idleProgressError", err)
		}
		if idle.phase != "test phase" {
			t.Fatalf("idle phase = %q, want %q", idle.phase, "test phase")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deadline did not fire after progress genuinely stopped")
	}
}

func assertIdlePhase(t *testing.T, err error, phase string) {
	t.Helper()
	var idle *idleProgressError
	if !errors.As(err, &idle) {
		t.Fatalf("error = %v, want idle progress timeout", err)
	}
	if idle.phase != phase {
		t.Fatalf("idle phase = %q, want %q", idle.phase, phase)
	}
	var timeout interface{ Timeout() bool }
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("error = %v, want timeout classification", err)
	}
}
