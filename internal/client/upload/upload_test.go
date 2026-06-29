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
)

// fakeServer is an in-memory stand-in for the akari ingest endpoints. It holds
// one session's transformed raw bytes and a content-addressed blob set, and
// enforces the same append-only, offset-checked, hash-reported protocol plus the
// client-CAS upload endpoints the real server implements. Under the new protocol
// the stored bytes are the TRANSFORMED transcript, so prefix_sha256 is the hash of
// buf and the client verifies its transformed prefix against it.
type fakeServer struct {
	mu    sync.Mutex
	buf   []byte
	blobs map[string][]byte // sha256 -> body bytes
	puts  int               // count of accepted blob uploads, for dedup assertions

	// conflictOnce, when set, makes the next chunk POST return 409 after first
	// appending injectBytes, simulating another writer advancing the cursor.
	conflictOnce bool
	injectBytes  []byte

	// alwaysConflict makes every chunk POST return 409, to exercise the retry cap.
	alwaysConflict bool
}

func (s *fakeServer) handler() http.Handler {
	if s.blobs == nil {
		s.blobs = map[string][]byte{}
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
		defer s.mu.Unlock()
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
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != sha {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": "hash mismatch"})
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.blobs[sha] = body
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

	ok, err := c.verifyPrefix(f, fs, "claude", 3, 4, hexSHA("BBB"))
	if err != nil || !ok {
		t.Fatalf("fast path against cached digest: ok=%v err=%v", ok, err)
	}
	// The same call must reject a hash that matches the on-disk "AAA", because the
	// fast path never looks at the file.
	ok, err = c.verifyPrefix(f, fs, "claude", 3, 4, hexSHA("AAA"))
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
	ok, err := c.verifyPrefix(f, fs, "claude", int64(len(first)), int64(len(content)), hexSHA(first))
	if err != nil || !ok {
		t.Fatalf("cold verify: ok=%v err=%v", ok, err)
	}
	if fs.base != int64(len(first)) || fs.origBase != int64(len(first)) {
		t.Fatalf("after cold verify base=%d origBase=%d, want %d/%d", fs.base, fs.origBase, len(first), len(first))
	}

	// A wrong hash is rejected.
	if ok, err := c.verifyPrefix(f, &fileSync{}, "claude", int64(len(first)), int64(len(content)), hexSHA("nope")); err != nil || ok {
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
