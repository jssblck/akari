package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/casenc"
	"github.com/jssblck/akari/internal/parser"
	"github.com/klauspost/compress/zstd"
)

// fakeServer is an in-memory stand-in for the akari ingest endpoints. It holds
// one session's transformed raw bytes and a content-addressed blob set, and
// enforces the same append-only, offset-checked, hash-reported protocol plus the
// client-CAS upload endpoints the real server implements. Under the new protocol
// the stored bytes are the TRANSFORMED transcript, so prefix_sha256 is the hash of
// buf and the client verifies its transformed prefix against it.
type fakeServer struct {
	mu        sync.Mutex
	buf       []byte
	blobs     map[string][]byte // sha256 -> stored (possibly compressed) body bytes
	blobCT    map[string]string // sha256 -> declared storage content_type
	blobMedia map[string]string // sha256 -> declared semantic media_type
	puts      int               // count of accepted blob uploads, for dedup assertions

	// Instrumentation for the batched/parallel upload tests. checkBatchSizes records the
	// hash count of every existence-check request, so a test can assert no request
	// exceeds the per-request cap. maxConcurrentChecks / maxConcurrentPuts record the
	// peak in-flight requests of each kind, so a test can assert the client actually
	// parallelizes. checkDelay / putDelay, when set, make that endpoint sleep so
	// concurrent requests overlap long enough for the peak to be observed.
	checkBatchSizes     []int
	curChecks           int
	maxConcurrentChecks int
	curPuts             int
	maxConcurrentPuts   int
	checkDelay          time.Duration
	putDelay            time.Duration

	// conflictOnce, when set, makes the next chunk POST return 409 after first
	// appending injectBytes, simulating another writer advancing the cursor.
	conflictOnce bool
	injectBytes  []byte

	// alwaysConflict makes every chunk POST return 409, to exercise the retry cap.
	alwaysConflict bool

	// failCheckStatus / failPutStatus, when non-zero, make the blob-check or blob-upload
	// endpoint return that HTTP status, to drive the client's error paths.
	failCheckStatus int
	failPutStatus   int
}

func (s *fakeServer) handler() http.Handler {
	if s.blobs == nil {
		s.blobs = map[string][]byte{}
	}
	if s.blobCT == nil {
		s.blobCT = map[string]string{}
	}
	if s.blobMedia == nil {
		s.blobMedia = map[string]string{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/ingest/session", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		sum := sha256.Sum256(s.buf)
		writeJSON(w, map[string]any{
			"session_id":    1,
			"stored_bytes":  len(s.buf),
			"prefix_sha256": hex.EncodeToString(sum[:]),
		})
	})
	mux.HandleFunc("POST /api/v1/ingest/session/{id}/chunk", func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		body := readAll(r)
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.alwaysConflict {
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, map[string]any{"error": "offset mismatch", "stored_bytes": len(s.buf)})
			return
		}
		if s.conflictOnce {
			s.conflictOnce = false
			s.buf = append(s.buf, s.injectBytes...) // another writer got there first
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, map[string]any{"error": "offset mismatch", "stored_bytes": len(s.buf)})
			return
		}
		if offset != len(s.buf) {
			w.WriteHeader(http.StatusConflict)
			writeJSON(w, map[string]any{"error": "offset mismatch", "stored_bytes": len(s.buf)})
			return
		}
		s.buf = append(s.buf, body...)
		writeJSON(w, map[string]any{"stored_bytes": len(s.buf), "message_count": bytes.Count(s.buf, []byte("\n"))})
	})
	mux.HandleFunc("POST /api/v1/ingest/session/{id}/reset", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.buf = nil
		writeJSON(w, map[string]any{"stored_bytes": 0})
	})
	mux.HandleFunc("POST /api/v1/ingest/blobs/check", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			SHA256 []string `json:"sha256"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		if s.failCheckStatus != 0 {
			status := s.failCheckStatus
			s.mu.Unlock()
			w.WriteHeader(status)
			writeJSON(w, map[string]any{"error": "check failed"})
			return
		}
		s.checkBatchSizes = append(s.checkBatchSizes, len(req.SHA256))
		s.curChecks++
		if s.curChecks > s.maxConcurrentChecks {
			s.maxConcurrentChecks = s.curChecks
		}
		delay := s.checkDelay
		s.mu.Unlock()
		// Sleep outside the lock so concurrent checks actually overlap and the peak
		// concurrency is observable.
		if delay > 0 {
			time.Sleep(delay)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.curChecks--
		missing := []string{}
		for _, sha := range req.SHA256 {
			if _, ok := s.blobs[sha]; !ok {
				missing = append(missing, sha)
			}
		}
		writeJSON(w, map[string]any{"missing": missing})
	})
	mux.HandleFunc("PUT /api/v1/ingest/blob/{sha256}", func(w http.ResponseWriter, r *http.Request) {
		sha := r.PathValue("sha256")
		body := readAll(r)
		s.mu.Lock()
		failPut := s.failPutStatus
		s.mu.Unlock()
		if failPut != 0 {
			w.WriteHeader(failPut)
			writeJSON(w, map[string]any{"error": "upload failed"})
			return
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != sha {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "hash mismatch"})
			return
		}
		s.mu.Lock()
		s.curPuts++
		if s.curPuts > s.maxConcurrentPuts {
			s.maxConcurrentPuts = s.curPuts
		}
		delay := s.putDelay
		s.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.curPuts--
		s.blobs[sha] = body
		s.blobCT[sha] = r.URL.Query().Get("content_type")
		s.blobMedia[sha] = r.URL.Query().Get("media_type")
		s.puts++
		writeJSON(w, map[string]any{"sha256": sha})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func readAll(r *http.Request) []byte {
	defer r.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(r.Body)
	return b.Bytes()
}

func newTestClient(t *testing.T) (*Client, *fakeServer) {
	t.Helper()
	fs := &fakeServer{}
	srv := httptest.NewServer(fs.handler())
	t.Cleanup(srv.Close)
	return New(srv.Client(), srv.URL, "test-token"), fs
}

func tempFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sess.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func target(path string) Target {
	return Target{Agent: "claude", Path: path, SourceID: "s1", ProjectKey: "github.com/o/r", Machine: "m"}
}

func TestSyncFresh(t *testing.T) {
	c, fs := newTestClient(t)
	content := "l1\nl2\nl3\n"
	out, err := c.SyncFile(context.Background(), target(tempFile(t, content)))
	if err != nil {
		t.Fatal(err)
	}
	if out.Action != ActionUploaded {
		t.Errorf("action = %s, want uploaded", out.Action)
	}
	if string(fs.buf) != content {
		t.Errorf("server buf = %q, want %q", fs.buf, content)
	}
	if out.UploadedBytes != int64(len(content)) {
		t.Errorf("uploaded %d, want %d", out.UploadedBytes, len(content))
	}
}

func TestSyncResume(t *testing.T) {
	c, fs := newTestClient(t)
	content := "l1\nl2\nl3\n"
	fs.buf = []byte("l1\n") // server already has the first line

	out, err := c.SyncFile(context.Background(), target(tempFile(t, content)))
	if err != nil {
		t.Fatal(err)
	}
	if out.Action != ActionUploaded {
		t.Errorf("action = %s, want uploaded", out.Action)
	}
	if out.UploadedBytes != int64(len("l2\nl3\n")) {
		t.Errorf("uploaded %d, want %d", out.UploadedBytes, len("l2\nl3\n"))
	}
	if string(fs.buf) != content {
		t.Errorf("server buf = %q", fs.buf)
	}
}

func TestSyncUpToDate(t *testing.T) {
	c, fs := newTestClient(t)
	content := "l1\nl2\n"
	fs.buf = []byte(content)

	out, err := c.SyncFile(context.Background(), target(tempFile(t, content)))
	if err != nil {
		t.Fatal(err)
	}
	if out.Action != ActionUpToDate {
		t.Errorf("action = %s, want uptodate", out.Action)
	}
	if out.UploadedBytes != 0 {
		t.Errorf("uploaded %d, want 0", out.UploadedBytes)
	}
}

func TestSyncDivergenceResets(t *testing.T) {
	c, fs := newTestClient(t)
	content := "l1\nl2\nl3\n"
	fs.buf = []byte("XX\n") // same length as the local prefix but different bytes

	out, err := c.SyncFile(context.Background(), target(tempFile(t, content)))
	if err != nil {
		t.Fatal(err)
	}
	if out.Action != ActionReset {
		t.Errorf("action = %s, want reset", out.Action)
	}
	if string(fs.buf) != content {
		t.Errorf("server buf = %q, want full re-upload %q", fs.buf, content)
	}
}

func TestSyncLocalShorterResets(t *testing.T) {
	c, fs := newTestClient(t)
	content := "l1\n"
	fs.buf = []byte("l1\nl2\nl3\n") // server holds more than the local file

	out, err := c.SyncFile(context.Background(), target(tempFile(t, content)))
	if err != nil {
		t.Fatal(err)
	}
	if out.Action != ActionReset {
		t.Errorf("action = %s, want reset", out.Action)
	}
	if string(fs.buf) != content {
		t.Errorf("server buf = %q, want %q", fs.buf, content)
	}
}

func TestSyncLeavesIncompleteTrailingLine(t *testing.T) {
	c, fs := newTestClient(t)
	content := "l1\nl2\npartial-no-newline"

	out, err := c.SyncFile(context.Background(), target(tempFile(t, content)))
	if err != nil {
		t.Fatal(err)
	}
	if string(fs.buf) != "l1\nl2\n" {
		t.Errorf("server buf = %q, want only complete lines", fs.buf)
	}
	if out.StoredBytes != int64(len("l1\nl2\n")) {
		t.Errorf("stored = %d", out.StoredBytes)
	}
}

func TestSyncReannouncesOnConflict(t *testing.T) {
	c, fs := newTestClient(t)
	content := "l1\nl2\n"
	// The first chunk POST conflicts after another writer appends "l1\n"; the
	// client must re-announce, see the prefix still matches, and upload only "l2\n".
	fs.conflictOnce = true
	fs.injectBytes = []byte("l1\n")

	out, err := c.SyncFile(context.Background(), target(tempFile(t, content)))
	if err != nil {
		t.Fatal(err)
	}
	if string(fs.buf) != content {
		t.Errorf("server buf = %q, want %q after re-announce", fs.buf, content)
	}
	if out.Action != ActionUploaded {
		t.Errorf("action = %s", out.Action)
	}
}

func TestSyncGivesUpAfterRepeatedConflicts(t *testing.T) {
	c, fs := newTestClient(t)
	fs.alwaysConflict = true
	_, err := c.SyncFile(context.Background(), target(tempFile(t, "l1\nl2\n")))
	if err == nil {
		t.Fatal("expected an error after repeated conflicts, got nil")
	}
}

func hexSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestPutBodyDeclaresContentTypeAndUploadsStoredBytes proves putBody PUTs the exact
// already-encoded stored bytes for an in-hand body and declares both the semantic
// media type and the storage content type, for a raw small body and a zstd-compressed
// one. The server keys on the bytes it receives, so the content type putBody declares
// must match the bytes it sends.
func TestPutBodyDeclaresContentTypeAndUploadsStoredBytes(t *testing.T) {
	c, fs := newTestClient(t)
	ctx := context.Background()
	enc := casenc.New()

	// Raw small body: stored verbatim, declared application/octet-stream.
	raw := []byte(`{"file_path":"a.go"}`)
	rawSHA, rawStored, rawCT := enc.EncodeBody(raw)
	rawRef := bodyRef{media: "application/json", kind: "input", haveContent: true, sha: rawSHA, stored: rawStored, contentType: rawCT, rawLen: len(raw)}
	if err := c.putBody(ctx, enc, rawSHA, rawCT, rawRef); err != nil {
		t.Fatalf("put raw body: %v", err)
	}
	if got := fs.blobs[rawSHA]; !bytes.Equal(got, raw) {
		t.Fatalf("server stored %q for the raw body, want the verbatim bytes", got)
	}
	if fs.blobCT[rawSHA] != parser.ContentRaw || fs.blobMedia[rawSHA] != "application/json" {
		t.Fatalf("raw body declared content_type=%q media_type=%q, want %q/application/json", fs.blobCT[rawSHA], fs.blobMedia[rawSHA], parser.ContentRaw)
	}

	// Compressed body: stored zstd, declared application/zstd, server keeps the
	// compressed bytes verbatim (it never decompresses).
	body := []byte(strings.Repeat("compress me ", 400))
	zSHA, zStored, zCT := enc.EncodeBody(body)
	if zCT != parser.ContentZstd {
		t.Fatalf("expected the large body to compress, got content type %q", zCT)
	}
	zRef := bodyRef{media: "text/plain", kind: "result", haveContent: true, sha: zSHA, stored: zStored, contentType: zCT, rawLen: len(body)}
	if err := c.putBody(ctx, enc, zSHA, zCT, zRef); err != nil {
		t.Fatalf("put compressed body: %v", err)
	}
	if got := fs.blobs[zSHA]; !bytes.Equal(got, zStored) {
		t.Fatal("server stored bytes differ from the compressed stored bytes putBody sent")
	}
	if fs.blobCT[zSHA] != parser.ContentZstd {
		t.Fatalf("compressed body declared content_type=%q, want %q", fs.blobCT[zSHA], parser.ContentZstd)
	}
}

// TestSyncUploadsCompressedBigBody drives a full sync of a transcript whose tool
// result is large and compressible: the streaming big-line path encodes it to zstd and
// putBody streams those compressed bytes to the server. This exercises putBody's
// streamed (not in-hand) branch, so the fake CAS must receive content_type=zstd and
// bytes that decode to the original.
func TestSyncUploadsCompressedBigBody(t *testing.T) {
	setBigLineThreshold(t, 256)
	c, fs := newTestClient(t)

	body := strings.Repeat("compress me ", 400)
	content := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"` + body + `"}]}}` + "\n"
	if _, err := c.SyncFile(context.Background(), target(tempFile(t, content))); err != nil {
		t.Fatal(err)
	}

	wantSHA, _, _ := casenc.New().EncodeBody([]byte(body))
	if fs.blobCT[wantSHA] != parser.ContentZstd {
		t.Fatalf("streamed big body content_type = %q, want %q", fs.blobCT[wantSHA], parser.ContentZstd)
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	got, err := dec.DecodeAll(fs.blobs[wantSHA], nil)
	if err != nil {
		t.Fatalf("decode stored bytes: %v", err)
	}
	if string(got) != body {
		t.Fatal("stored compressed bytes do not decode to the original body")
	}
}

// TestVerifyPrefixUsesCachedDigest proves the fast path compares the cached digest
// instead of re-transforming the prefix: the on-disk bytes differ from what the
// cache claims, yet verification follows the cache.
func TestVerifyPrefixUsesCachedDigest(t *testing.T) {
	c, _ := newTestClient(t)
	f, err := os.Open(tempFile(t, "AAA\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	fs := &fileSync{base: 3, prefixSize: 4, prefixHasher: sha256.New()}
	fs.prefixHasher.Write([]byte("BBB")) // cache claims the first 3 transformed bytes were "BBB"

	ok, err := c.verifyPrefix(context.Background(), f, fs, "claude", 3, 4, hexSHA("BBB"))
	if err != nil || !ok {
		t.Fatalf("fast path against cached digest: ok=%v err=%v", ok, err)
	}
	// The same call must reject a hash that matches the on-disk "AAA", because the
	// fast path never looks at the file.
	ok, err = c.verifyPrefix(context.Background(), f, fs, "claude", 3, 4, hexSHA("AAA"))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("fast path should compare the cached digest, not re-read the file")
	}
}

// TestVerifyPrefixColdReTransforms proves the cold path re-transforms the original
// file from zero to verify the transformed prefix and recover the original cursor.
// The transcript here carries no tool body, so the transform is identity and the
// transformed prefix equals the raw prefix.
func TestVerifyPrefixColdReTransforms(t *testing.T) {
	c, _ := newTestClient(t)
	content := claudeLine("hi") + claudeLine("bye")
	path := tempFile(t, content)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	first := claudeLine("hi")
	fs := &fileSync{}
	ok, err := c.verifyPrefix(context.Background(), f, fs, "claude", int64(len(first)), int64(len(content)), hexSHA(first))
	if err != nil || !ok {
		t.Fatalf("cold verify: ok=%v err=%v", ok, err)
	}
	if fs.base != int64(len(first)) || fs.origBase != int64(len(first)) {
		t.Fatalf("after cold verify base=%d origBase=%d, want %d/%d", fs.base, fs.origBase, len(first), len(first))
	}

	// A wrong hash is rejected.
	if ok, err := c.verifyPrefix(context.Background(), f, &fileSync{}, "claude", int64(len(first)), int64(len(content)), hexSHA("nope")); err != nil || ok {
		t.Fatalf("cold verify of wrong hash: ok=%v err=%v, want false", ok, err)
	}
}

// claudeLine builds a one-line Claude user message with the given text, newline
// terminated, for protocol tests that need realistic JSONL the transform passes
// through unchanged (no tool body).
func claudeLine(text string) string {
	return `{"type":"user","message":{"content":` + jsonString(text) + `}}` + "\n"
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestConcurrentSyncSamePathSerializes runs many syncs of one path at once. The
// per-file lock must serialize them so they cannot corrupt the shared cursor and
// hasher; the server ends up with exactly the file content. Run under -race to
// catch a regression in the locking.
func TestConcurrentSyncSamePathSerializes(t *testing.T) {
	c, fs := newTestClient(t)
	content := strings.Repeat("line\n", 64)
	path := tempFile(t, content)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.SyncFile(context.Background(), target(path)); err != nil {
				t.Errorf("sync: %v", err)
			}
		}()
	}
	wg.Wait()

	if string(fs.buf) != content {
		t.Fatalf("server buf = %q, want the file content once", fs.buf)
	}
}

// TestSyncFileLockWaitIsCancellable proves the per-file single-flight wait honors
// the context: while one holder has the lock, a caller whose context is already
// canceled returns promptly instead of blocking behind the in-flight sync.
func TestSyncFileLockWaitIsCancellable(t *testing.T) {
	c, _ := newTestClient(t)
	path := tempFile(t, "l1\n")

	// Take the per-file lock and keep it, so the SyncFile below must wait for it.
	state := c.fileState(path)
	state.lock <- struct{}{}
	defer func() { <-state.lock }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.SyncFile(ctx, target(path)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
