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
	mu           sync.Mutex
	buf          []byte
	lastAnnounce map[string]any    // the most recent announce request body, decoded
	finalizes    int               // count of finalize POSTs, for the --finalize refresh assertion
	blobs        map[string][]byte // sha256 -> stored (possibly compressed) body bytes
	blobCT       map[string]string // sha256 -> declared storage content_type
	blobMedia    map[string]string // sha256 -> declared semantic media_type
	puts         int               // count of accepted blob uploads, for dedup assertions

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
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.lastAnnounce = body
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
	mux.HandleFunc("POST /api/v1/ingest/session/{id}/finalize", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.finalizes++
		writeJSON(w, map[string]any{"finalized": true})
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

// TestAnnounceSerializesTargetFields guards the announce request wire shape, in
// particular that a standalone session's LocalRoot is sent as "local_root": a
// misspelled or dropped field would silently defeat the worktree grouping, since
// the server would just fall back to keying on cwd.
func TestAnnounceSerializesTargetFields(t *testing.T) {
	c, fs := newTestClient(t)
	tgt := Target{
		Agent:     "claude",
		Path:      tempFile(t, "l1\n"),
		SourceID:  "s1",
		Kind:      "standalone",
		LocalRoot: "/home/grace/repo",
		GitBranch: "feature-a",
		Cwd:       "/home/grace/wt/feature-a",
		Machine:   "grace-laptop",
	}
	if _, err := c.SyncFile(context.Background(), tgt); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"agent":             "claude",
		"source_session_id": "s1",
		"kind":              "standalone",
		"local_root":        "/home/grace/repo",
		"git_branch":        "feature-a",
		"cwd":               "/home/grace/wt/feature-a",
		"machine":           "grace-laptop",
		// An ordinary (non-finalize) sync announces terminal=false, so the server keeps
		// the idle-window grading behavior.
		"terminal": false,
	}
	for k, v := range want {
		if got := fs.lastAnnounce[k]; got != v {
			t.Errorf("announce[%q] = %v, want %v", k, got, v)
		}
	}
}

// TestFinalizeAnnouncesTerminalAndRefreshes covers the client half of the server-side
// --finalize fix: a finalized sync announces the session terminal and, once the whole
// transcript has landed, calls the finalize endpoint so the server grades it now. A
// non-finalized sync does neither, so ordinary syncs keep the idle-window behavior.
func TestFinalizeAnnouncesTerminalAndRefreshes(t *testing.T) {
	for _, finalize := range []bool{true, false} {
		c, fs := newTestClient(t)
		tgt := target(tempFile(t, "l1\nl2\n"))
		tgt.Finalize = finalize
		if _, err := c.SyncFile(context.Background(), tgt); err != nil {
			t.Fatalf("finalize=%v: SyncFile: %v", finalize, err)
		}
		if got := fs.lastAnnounce["terminal"]; got != finalize {
			t.Errorf("finalize=%v: announce[terminal] = %v, want %v", finalize, got, finalize)
		}
		wantFinalizes := 0
		if finalize {
			wantFinalizes = 1
		}
		if fs.finalizes != wantFinalizes {
			t.Errorf("finalize=%v: finalize calls = %d, want %d", finalize, fs.finalizes, wantFinalizes)
		}
	}
}

// TestFinalizeRefreshesEvenWhenUpToDate proves the finalize refresh does not depend on
// bytes moving: a session already fully on the server (a re-sync) still triggers the
// grade, since an ephemeral host may finalize a transcript it uploaded on an earlier
// tick that was never graded.
func TestFinalizeRefreshesEvenWhenUpToDate(t *testing.T) {
	c, fs := newTestClient(t)
	path := tempFile(t, "l1\nl2\n")
	if _, err := c.SyncFile(context.Background(), target(path)); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	if fs.finalizes != 0 {
		t.Fatalf("non-finalize sync triggered %d finalizes, want 0", fs.finalizes)
	}
	tgt := target(path)
	tgt.Finalize = true
	out, err := c.SyncFile(context.Background(), tgt)
	if err != nil {
		t.Fatalf("finalize re-sync: %v", err)
	}
	if out.Action != ActionUpToDate {
		t.Errorf("action = %s, want uptodate (nothing new to send)", out.Action)
	}
	if fs.finalizes != 1 {
		t.Errorf("up-to-date finalize triggered %d finalizes, want 1", fs.finalizes)
	}
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

// TestVerifyPrefixReDerivesWarmState proves a cached digest cannot hide an
// in-place rewrite. Verification follows the current file even when the cached
// cursor and size still line up with the server.
func TestVerifyPrefixReDerivesWarmState(t *testing.T) {
	c, _ := newTestClient(t)
	f, err := os.Open(tempFile(t, "AAA\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	fs := &fileSync{base: 4, origBase: 4, prefixSize: 4, prefixHasher: sha256.New()}
	fs.prefixHasher.Write([]byte("BBB\n"))

	ok, err := c.verifyPrefix(context.Background(), f, fs, "claude", 4, 4, hexSHA("BBB\n"))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("verification trusted a cached digest that differs from the file")
	}
	ok, err = c.verifyPrefix(context.Background(), f, fs, "claude", 4, 4, hexSHA("AAA\n"))
	if err != nil || !ok {
		t.Fatalf("verification against current file: ok=%v err=%v", ok, err)
	}
}

func TestSyncWarmSameLengthRewriteResets(t *testing.T) {
	c, fs := newTestClient(t)
	path := tempFile(t, "old\n")
	if _, err := c.SyncFile(context.Background(), target(path)); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	if err := os.WriteFile(path, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := c.SyncFile(context.Background(), target(path))
	if err != nil {
		t.Fatal(err)
	}
	if out.Action != ActionReset {
		t.Fatalf("action = %s, want reset", out.Action)
	}
	if got := string(fs.buf); got != "new\n" {
		t.Fatalf("server buf = %q, want rewritten file", got)
	}
}

func TestSyncInvalidatesPendingTurnAfterSameInodeRewrite(t *testing.T) {
	c, server := newTestClient(t)
	path := tempFile(t, codexAsst("old"))
	target := Target{Agent: "codex", Path: path, SourceID: "s1", ProjectKey: "github.com/o/r", Machine: "m"}

	if _, err := c.SyncFile(context.Background(), target); err != nil {
		t.Fatalf("cache open turn: %v", err)
	}
	if got := string(server.buf); got != "" {
		t.Fatalf("unsettled open turn uploaded early: %q", got)
	}

	// Truncate and rewrite the same filesystem object without changing its size.
	// The cached transformed turn must not survive merely because the inode and
	// length still match.
	want := codexAsst("new")
	if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	target.Finalize = true
	if _, err := c.SyncFile(context.Background(), target); err != nil {
		t.Fatalf("finalize rewritten turn: %v", err)
	}
	if got := string(server.buf); got != want {
		t.Fatalf("server buf = %q, want current on-disk turn %q", got, want)
	}
}

func TestSyncInvalidatesPartialLineSearchAfterEarlierNewline(t *testing.T) {
	c, server := newTestClient(t)
	path := tempFile(t, "base\nfirst")
	target := target(path)

	if _, err := c.SyncFile(context.Background(), target); err != nil {
		t.Fatalf("cache partial line search: %v", err)
	}
	if got, want := string(server.buf), "base\n"; got != want {
		t.Fatalf("initial server buf = %q, want complete prefix %q", got, want)
	}

	// Insert a newline before the old search cursor while retaining the old bytes.
	// Resuming blindly from the cached offset skips the newly completed first line.
	if err := os.WriteFile(path, []byte("base\nx\nfirst"), 0o644); err != nil {
		t.Fatal(err)
	}
	target.Finalize = true
	if _, err := c.SyncFile(context.Background(), target); err != nil {
		t.Fatalf("sync rewritten partial line: %v", err)
	}
	if got, want := string(server.buf), "base\nx\n"; got != want {
		t.Fatalf("server buf = %q, want newly completed line %q", got, want)
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

func TestSyncFileOpensAfterLockWait(t *testing.T) {
	c, fs := newTestClient(t)
	path := tempFile(t, "old\n")

	state := &fileSync{lock: make(chan struct{}, 1)}
	state.lock <- struct{}{}
	c.files[path] = state

	type result struct {
		out Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := c.SyncFile(context.Background(), target(path))
		done <- result{out: out, err: err}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		refs := state.refs
		c.mu.Unlock()
		if refs > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sync did not reach the file-state lock")
		}
		time.Sleep(time.Millisecond)
	}

	replacement := filepath.Join(filepath.Dir(path), "replacement.jsonl")
	if err := os.WriteFile(replacement, []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	<-state.lock

	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	if got := string(fs.buf); got != "new\n" {
		t.Fatalf("server buf = %q, want replacement file", got)
	}
}

func TestSyncFileInvalidatesPendingTurnOnIdentityChange(t *testing.T) {
	tests := []struct {
		name    string
		replace bool
		newID   string
	}{
		{name: "logical session", newID: "s2"},
		{name: "filesystem object", replace: true, newID: "s1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, fs := newTestClient(t)
			path := tempFile(t, codexAsst("old"))
			first := Target{Agent: "codex", Path: path, SourceID: "s1", ProjectKey: "github.com/o/r", Machine: "m"}
			if _, err := c.SyncFile(context.Background(), first); err != nil {
				t.Fatalf("initial open turn: %v", err)
			}
			if len(fs.buf) != 0 {
				t.Fatalf("open turn uploaded early: %q", fs.buf)
			}

			content := codexAsst("new")
			if tt.replace {
				replacement := filepath.Join(filepath.Dir(path), "replacement.jsonl")
				if err := os.WriteFile(replacement, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			next := first
			next.SourceID = tt.newID
			next.Finalize = true
			if _, err := c.SyncFile(context.Background(), next); err != nil {
				t.Fatal(err)
			}
			if got := string(fs.buf); got != content {
				t.Fatalf("server buf = %q, want only replacement turn %q", got, content)
			}
		})
	}
}

func TestFileStateCacheEvictsOnlyIdleEntries(t *testing.T) {
	oldCap := fileStateCacheCap
	fileStateCacheCap = 2
	t.Cleanup(func() { fileStateCacheCap = oldCap })

	c, _ := newTestClient(t)
	ctx := context.Background()
	held, err := c.acquireFileState(ctx, "held")
	if err != nil {
		t.Fatal(err)
	}
	idle, err := c.acquireFileState(ctx, "idle")
	if err != nil {
		t.Fatal(err)
	}
	c.releaseFileState(idle)
	newest, err := c.acquireFileState(ctx, "newest")
	if err != nil {
		t.Fatal(err)
	}
	c.releaseFileState(newest)

	c.mu.Lock()
	_, heldPresent := c.files["held"]
	_, idlePresent := c.files["idle"]
	size := len(c.files)
	c.mu.Unlock()
	if !heldPresent {
		t.Fatal("evicted an in-use file state")
	}
	if idlePresent {
		t.Fatal("least-recently-used idle state was not evicted")
	}
	if size > fileStateCacheCap {
		t.Fatalf("cache size = %d, cap = %d", size, fileStateCacheCap)
	}
	c.releaseFileState(held)
}

func TestEvictedFileStateRebuildsFromServerPrefix(t *testing.T) {
	oldCap := fileStateCacheCap
	fileStateCacheCap = 1
	t.Cleanup(func() { fileStateCacheCap = oldCap })

	c, fs := newTestClient(t)
	path := tempFile(t, "first\n")
	if _, err := c.SyncFile(context.Background(), target(path)); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	other, err := c.acquireFileState(context.Background(), "other")
	if err != nil {
		t.Fatal(err)
	}
	c.releaseFileState(other)
	c.mu.Lock()
	_, retained := c.files[path]
	c.mu.Unlock()
	if retained {
		t.Fatal("least-recently-used state was not evicted")
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("second\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SyncFile(context.Background(), target(path)); err != nil {
		t.Fatalf("cold sync after eviction: %v", err)
	}
	if got, want := string(fs.buf), "first\nsecond\n"; got != want {
		t.Fatalf("server buf = %q, want %q", got, want)
	}
}

// TestSyncFileLockWaitIsCancellable proves the per-file single-flight wait honors
// the context: while one holder has the lock, a caller whose context is already
// canceled returns promptly instead of blocking behind the in-flight sync.
func TestSyncFileLockWaitIsCancellable(t *testing.T) {
	c, _ := newTestClient(t)
	path := tempFile(t, "l1\n")

	// Take the per-file lock and keep it, so the SyncFile below must wait for it.
	state := &fileSync{lock: make(chan struct{}, 1)}
	c.files[path] = state
	state.lock <- struct{}{}
	defer func() { <-state.lock }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.SyncFile(ctx, target(path)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
