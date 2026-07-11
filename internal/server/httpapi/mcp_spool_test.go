package httpapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/jssblck/akari/internal/server/auth"
)

func newTestMCPSpooler(t *testing.T, next http.Handler, maxBytes, memoryBytes int64, maxSpools int) *mcpBodySpooler {
	t.Helper()
	s, err := newMCPBodySpoolerAt(next, t.TempDir(), maxSpools)
	if err != nil {
		t.Fatalf("new spooler: %v", err)
	}
	s.maxBytes = maxBytes
	s.memoryBytes = memoryBytes
	return s
}

func mcpPost(body io.Reader) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/mcp", body)
	req.Header.Set("Content-Type", "application/json")
	return req
}

type countingReadCloser struct {
	r     io.Reader
	reads atomic.Int64
}

func (r *countingReadCloser) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.reads.Add(int64(n))
	return n, err
}

func (*countingReadCloser) Close() error { return nil }

func TestMCPBodySpoolerRejectsContentLengthWithoutReading(t *testing.T) {
	body := &countingReadCloser{r: bytes.NewReader([]byte("{}"))}
	req := mcpPost(body)
	req.ContentLength = mcpMaxRequestBytes + 1
	reached := false
	spooler := newTestMCPSpooler(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}), mcpMaxRequestBytes, 32, 1)

	w := httptest.NewRecorder()
	spooler.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
	if got := body.reads.Load(); got != 0 {
		t.Fatalf("read %d bytes from declared-oversized body, want 0", got)
	}
	if reached {
		t.Fatal("oversized request reached downstream handler")
	}
	if !req.Close || w.Header().Get("Connection") != "close" {
		t.Fatal("oversized response did not disable connection reuse")
	}
}

func TestMCPEndpointRejectsOversizedAuthenticatedRequestBeforeReading(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	user, err := st.Register(ctx, "anna", mustHash(t, "winlock-1857"), "")
	if err != nil {
		t.Fatalf("register Anna Winlock: %v", err)
	}
	secret, err := auth.NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	if _, err := st.CreateAPIToken(ctx, user.ID, "read token", "read", auth.HashToken(secret)); err != nil {
		t.Fatalf("create read token: %v", err)
	}

	body := &countingReadCloser{r: bytes.NewReader([]byte("{}"))}
	req := mcpPost(body)
	req.ContentLength = mcpMaxRequestBytes + 1
	req.Header.Set("Authorization", "Bearer "+secret)
	w := httptest.NewRecorder()
	srv.Config.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
	if got := body.reads.Load(); got != 0 {
		t.Fatalf("read %d bytes from declared-oversized body, want 0", got)
	}
}

func TestMCPBodySpoolerRejectsChunkedBodyAfterOneExtraByte(t *testing.T) {
	const maxBytes = int64(64 << 10)
	src := &countingReadCloser{r: io.LimitReader(zeroReader{}, maxBytes+4096)}
	req := mcpPost(src)
	req.ContentLength = -1
	spooler := newTestMCPSpooler(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("oversized chunked request reached downstream handler")
	}), maxBytes, 1024, 1)

	w := httptest.NewRecorder()
	spooler.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
	}
	if got := src.reads.Load(); got != maxBytes+1 {
		t.Fatalf("read %d bytes, want exactly limit+1 (%d)", got, maxBytes+1)
	}
	assertNoMCPSpools(t, spooler.dir)
}

func TestMCPBodySpoolerAcceptsBodyAtLimit(t *testing.T) {
	const maxBytes = int64(4096)
	payload := bytes.Repeat([]byte("x"), int(maxBytes))
	received := 0
	spooler := newTestMCPSpooler(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		received = len(body)
	}), maxBytes, 32, 1)

	spooler.ServeHTTP(httptest.NewRecorder(), mcpPost(bytes.NewReader(payload)))

	if received != len(payload) {
		t.Fatalf("received %d bytes, want %d", received, len(payload))
	}
	assertNoMCPSpools(t, spooler.dir)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func TestMCPBodySpoolerRewindsLargeAcceptedBodyFromProtectedFile(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), 8192)
	var spoolPath string
	spooler := newTestMCPSpooler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		file, ok := r.Body.(*os.File)
		if !ok {
			t.Fatalf("downstream body = %T, want *os.File", r.Body)
		}
		spoolPath = file.Name()
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read spooled body: %v", err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("spooled body differs: got %d bytes, want %d", len(got), len(payload))
		}
		info, err := os.Stat(spoolPath)
		if err != nil {
			t.Fatalf("stat live spool: %v", err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Fatalf("spool mode = %o, want 600", info.Mode().Perm())
		}
		w.WriteHeader(http.StatusNoContent)
	}), 16<<10, 1024, 1)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(spooler.dir)
		if err != nil {
			t.Fatalf("stat spool directory: %v", err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Fatalf("spool directory mode = %o, want 700", info.Mode().Perm())
		}
	}

	w := httptest.NewRecorder()
	spooler.ServeHTTP(w, mcpPost(bytes.NewReader(payload)))

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusNoContent, w.Body.String())
	}
	if _, err := os.Stat(spoolPath); !os.IsNotExist(err) {
		t.Fatalf("spool still exists after success: %v", err)
	}
	assertNoMCPSpools(t, spooler.dir)
}

func TestMCPBodySpoolerKeepsSmallBodyInMemory(t *testing.T) {
	payload := []byte(`{"jsonrpc":"2.0"}`)
	spooler := newTestMCPSpooler(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if _, ok := r.Body.(*os.File); ok {
			t.Fatal("small request was spooled to disk")
		}
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("body = %q, err %v", got, err)
		}
	}), 4096, 1024, 1)

	spooler.ServeHTTP(httptest.NewRecorder(), mcpPost(bytes.NewReader(payload)))
	assertNoMCPSpools(t, spooler.dir)
}

func TestMCPBodySpoolerCleansFileAfterDownstreamError(t *testing.T) {
	spooler := newTestMCPSpooler(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "bad request", http.StatusBadRequest)
	}), 4096, 32, 1)

	w := httptest.NewRecorder()
	spooler.ServeHTTP(w, mcpPost(bytes.NewReader(bytes.Repeat([]byte("x"), 512))))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	assertNoMCPSpools(t, spooler.dir)
}

func TestMCPBodySpoolerCleansFileAfterCancellation(t *testing.T) {
	entered := make(chan struct{})
	spooler := newTestMCPSpooler(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
	}), 4096, 32, 1)
	ctx, cancel := context.WithCancel(context.Background())
	req := mcpPost(bytes.NewReader(bytes.Repeat([]byte("x"), 512))).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		spooler.ServeHTTP(httptest.NewRecorder(), req)
		close(done)
	}()

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("downstream handler was not reached")
	}
	if got := countMCPSpools(t, spooler.dir); got != 1 {
		t.Fatalf("live spool count = %d, want 1", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after cancellation")
	}
	assertNoMCPSpools(t, spooler.dir)
}

func TestMCPBodySpoolerCleansPartialFileWhenUploadIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	src := &cancelAfterReader{cancel: cancel, remaining: 256}
	reached := false
	spooler := newTestMCPSpooler(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}), 4096, 32, 1)

	spooler.ServeHTTP(httptest.NewRecorder(), mcpPost(src).WithContext(ctx))

	if reached {
		t.Fatal("canceled partial request reached downstream handler")
	}
	assertNoMCPSpools(t, spooler.dir)
	if got := len(spooler.slots); got != 0 {
		t.Fatalf("occupied slots = %d, want 0", got)
	}
}

type cancelAfterReader struct {
	cancel    context.CancelFunc
	remaining int
}

func (r *cancelAfterReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.remaining -= n
	if r.remaining == 0 {
		r.cancel()
	}
	return n, nil
}

func TestMCPBodySpoolerReportsDiskFailureAndReleasesSlot(t *testing.T) {
	reached := false
	spooler := newTestMCPSpooler(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}), 4096, 32, 1)
	if err := os.Remove(spooler.dir); err != nil {
		t.Fatalf("remove spool directory: %v", err)
	}

	w := httptest.NewRecorder()
	spooler.ServeHTTP(w, mcpPost(bytes.NewReader(bytes.Repeat([]byte("x"), 512))))

	if w.Code != http.StatusInsufficientStorage {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusInsufficientStorage)
	}
	if reached {
		t.Fatal("request reached downstream after disk failure")
	}
	if got := len(spooler.slots); got != 0 {
		t.Fatalf("occupied slots = %d, want 0", got)
	}
}

func TestUnavailableMCPBodySpoolerRejectsOnlyJSONPosts(t *testing.T) {
	var reached atomic.Int64
	handler := unavailableMCPBodySpooler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))

	post := httptest.NewRecorder()
	handler.ServeHTTP(post, mcpPost(bytes.NewReader([]byte("{}"))))
	if post.Code != http.StatusInsufficientStorage {
		t.Fatalf("POST status = %d, want %d", post.Code, http.StatusInsufficientStorage)
	}

	get := httptest.NewRecorder()
	handler.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if get.Code != http.StatusNoContent {
		t.Fatalf("GET status = %d, want %d", get.Code, http.StatusNoContent)
	}
	if got := reached.Load(); got != 1 {
		t.Fatalf("downstream calls = %d, want 1", got)
	}
}

func TestMCPBodySpoolerBoundsConcurrentFiles(t *testing.T) {
	const maxSpools = 2
	release := make(chan struct{})
	entered := make(chan struct{}, maxSpools+1)
	spooler := newTestMCPSpooler(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		entered <- struct{}{}
		<-release
	}), 4096, 32, maxSpools)

	var wg sync.WaitGroup
	for range maxSpools {
		wg.Add(1)
		go func() {
			defer wg.Done()
			spooler.ServeHTTP(httptest.NewRecorder(), mcpPost(bytes.NewReader(bytes.Repeat([]byte("x"), 512))))
		}()
	}
	for range maxSpools {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("admitted request did not reach downstream")
		}
	}
	queuedBody := &countingReadCloser{r: bytes.NewReader(bytes.Repeat([]byte("x"), 512))}
	queuedStarted := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		close(queuedStarted)
		spooler.ServeHTTP(httptest.NewRecorder(), mcpPost(queuedBody))
	}()
	<-queuedStarted
	select {
	case <-entered:
		t.Fatal("request exceeded concurrent spool budget")
	default:
	}
	if got := queuedBody.reads.Load(); got != 0 {
		t.Fatalf("queued request read %d bytes before admission", got)
	}
	if got := countMCPSpools(t, spooler.dir); got != maxSpools {
		t.Fatalf("live spool count = %d, want %d", got, maxSpools)
	}

	release <- struct{}{}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("queued request did not enter after capacity was released")
	}
	release <- struct{}{}
	release <- struct{}{}
	wg.Wait()
	assertNoMCPSpools(t, spooler.dir)
}

func TestMCPBodySpoolerRecoversAbandonedFilesWithoutTouchingLiveOnes(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "request-stale.body")
	live := filepath.Join(dir, "request-live.body")
	for _, path := range []string{stale, live} {
		if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
	}
	liveLockPath := spoolLockPath(live)
	liveLock := flock.New(liveLockPath)
	locked, err := liveLock.TryLock()
	if err != nil || !locked {
		t.Fatalf("lock live spool: locked=%v err=%v", locked, err)
	}
	t.Cleanup(func() {
		_ = liveLock.Unlock()
		_ = liveLock.Close()
		_ = os.Remove(liveLockPath)
	})

	_, err = newMCPBodySpoolerAt(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}), dir, 1)
	if err != nil {
		t.Fatalf("new spooler: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("abandoned spool still exists: %v", err)
	}
	if _, err := os.Stat(spoolLockPath(stale)); !os.IsNotExist(err) {
		t.Fatalf("abandoned spool lock still exists: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("live spool was removed: %v", err)
	}
}

func assertNoMCPSpools(t *testing.T, dir string) {
	t.Helper()
	if got := countMCPSpools(t, dir); got != 0 {
		t.Fatalf("spool count = %d, want 0", got)
	}
	locks, err := filepath.Glob(filepath.Join(dir, mcpSpoolLockPattern))
	if err != nil {
		t.Fatalf("glob spool locks: %v", err)
	}
	if len(locks) != 0 {
		t.Fatalf("spool lock count = %d, want 0", len(locks))
	}
}

func countMCPSpools(t *testing.T, dir string) int {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, mcpSpoolFilePattern))
	if err != nil {
		t.Fatalf("glob spools: %v", err)
	}
	return len(paths)
}
