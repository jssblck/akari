package upload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
// one session's raw bytes and enforces the same append-only, offset-checked,
// hash-reported protocol the real server implements.
type fakeServer struct {
	mu  sync.Mutex
	buf []byte

	// conflictOnce, when set, makes the next chunk POST return 409 after first
	// appending injectBytes, simulating another writer advancing the cursor.
	conflictOnce bool
	injectBytes  []byte

	// alwaysConflict makes every chunk POST return 409, to exercise the retry cap.
	alwaysConflict bool
}

func (s *fakeServer) handler() http.Handler {
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

func TestNextChunkTrimsToNewline(t *testing.T) {
	path := tempFile(t, "a\nb\nc")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()

	chunk, _, err := nextChunk(f, 0, 0, info.Size(), "claude", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(chunk) != "a\nb\n" {
		t.Errorf("chunk = %q, want a\\nb\\n", chunk)
	}

	// From the trailing "c" with no newline, there is nothing complete to send.
	tail, _, err := nextChunk(f, int64(len("a\nb\n")), int64(len("a\nb\n")), info.Size(), "claude", false)
	if err != nil {
		t.Fatal(err)
	}
	if tail != nil {
		t.Errorf("tail = %q, want nil (incomplete line)", tail)
	}
}

func TestNextChunkGrowsForLongLine(t *testing.T) {
	// A single line larger than chunkTarget forces the window to grow until it
	// reaches the newline.
	long := strings.Repeat("x", chunkTarget+1<<20) + "\n"
	content := long + "short\n"
	path := tempFile(t, content)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()

	chunk, _, err := nextChunk(f, 0, 0, info.Size(), "claude", false)
	if err != nil {
		t.Fatal(err)
	}
	// The window had to grow past chunkTarget to find any newline; once grown it
	// returns through the last newline that fits, which is the whole file here.
	if len(chunk) <= chunkTarget {
		t.Errorf("chunk length = %d, expected growth past chunkTarget %d", len(chunk), chunkTarget)
	}
	if string(chunk) != content {
		t.Errorf("grown chunk should cover the whole file: got %d bytes, want %d", len(chunk), len(content))
	}
}

// TestNextChunkCodexCutsAtTurnBoundary checks that a Codex chunk ends right after
// a user line (a turn boundary), keeping each folded turn whole and withholding
// the trailing in-progress turn until the file settles.
func TestNextChunkCodexCutsAtTurnBoundary(t *testing.T) {
	lines := []string{
		`{"type":"session_meta","payload":{"cwd":"/x"}}`,
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"a"}]}}`,
		`{"type":"response_item","payload":{"type":"reasoning","content":[{"type":"text","text":"r"}]}}`,
		`{"type":"response_item","payload":{"role":"assistant","content":[{"type":"output_text","text":"x"}]}}`,
		`{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"b"}]}}`,
		`{"type":"response_item","payload":{"role":"assistant","content":[{"type":"output_text","text":"y"}]}}`,
	}
	content := strings.Join(lines, "\n") + "\n"
	path := tempFile(t, content)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()

	// The cut falls right after the second user line; the trailing assistant turn
	// (lines[5]) is withheld because its closing user line has not arrived.
	wantCut := len(strings.Join(lines[:5], "\n") + "\n")
	chunk, _, err := nextChunk(f, 0, 0, info.Size(), "codex", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunk) != wantCut {
		t.Fatalf("chunk len = %d, want %d (through the second user line)", len(chunk), wantCut)
	}

	// The trailing turn stays withheld while the file is still considered live.
	tail, _, err := nextChunk(f, int64(wantCut), int64(wantCut), info.Size(), "codex", false)
	if err != nil {
		t.Fatal(err)
	}
	if tail != nil {
		t.Errorf("unsettled trailing turn = %q, want nil", tail)
	}

	// Once the file has settled, the final turn is flushed whole.
	flushed, _, err := nextChunk(f, int64(wantCut), int64(wantCut), info.Size(), "codex", true)
	if err != nil {
		t.Fatal(err)
	}
	if string(flushed) != lines[5]+"\n" {
		t.Errorf("settled flush = %q, want %q", flushed, lines[5]+"\n")
	}
}

// TestCodexChunkIncrementalScan checks that boundary detection resumes from
// scanFrom instead of rescanning from offset: a turn whose earlier bytes were
// already examined (and found to hold no boundary) is completed by scanning only
// the newly appended tail, yet the returned chunk still covers from offset.
func TestCodexChunkIncrementalScan(t *testing.T) {
	user := `{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"a"}]}}`
	reasoning := `{"type":"response_item","payload":{"type":"reasoning","content":[{"type":"text","text":"r"}]}}`
	assistant := `{"type":"response_item","payload":{"role":"assistant","content":[{"type":"output_text","text":"x"}]}}`
	closing := `{"type":"response_item","payload":{"role":"user","content":[{"type":"input_text","text":"b"}]}}`

	// [reasoning, assistant] is an in-progress turn with no boundary; the closing
	// user line lands in the tail. scanFrom sits at the end of the in-progress turn,
	// modelling a prior tick that scanned that far and withheld.
	head := reasoning + "\n" + assistant + "\n"
	content := head + closing + "\n"
	path := tempFile(t, content)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	size := info.Size()

	chunk, scannedTo, err := nextChunk(f, 0, int64(len(head)), size, "codex", false)
	if err != nil {
		t.Fatal(err)
	}
	// Even though the scan started past [reasoning, assistant], the chunk covers the
	// whole turn from offset 0 through the closing user line.
	if string(chunk) != content {
		t.Errorf("chunk = %q, want the whole turn %q", chunk, content)
	}
	if scannedTo != size {
		t.Errorf("scannedTo = %d, want %d", scannedTo, size)
	}

	// A user line sitting before scanFrom is trusted-already-scanned and so is not
	// treated as a fresh boundary: with the cursor past the first user line, the cut
	// is the later one, not the earlier.
	twoUsers := user + "\n" + reasoning + "\n" + closing + "\n"
	path2 := tempFile(t, twoUsers)
	f2, err := os.Open(path2)
	if err != nil {
		t.Fatal(err)
	}
	defer f2.Close()
	size2 := int64(len(twoUsers))
	// scanFrom just past the first user line: detection should find only the closing
	// user line and return the whole range from offset 0.
	chunk2, _, err := nextChunk(f2, 0, int64(len(user)+1), size2, "codex", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(chunk2) != twoUsers {
		t.Errorf("chunk2 = %q, want %q", chunk2, twoUsers)
	}
}

func hexSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestVerifyPrefixUsesCachedDigest proves the fast path compares the cached digest
// instead of re-reading the prefix: the on-disk bytes differ from what the cache
// claims, yet verification follows the cache, so no rehash happened.
func TestVerifyPrefixUsesCachedDigest(t *testing.T) {
	c, _ := newTestClient(t)
	f, err := os.Open(tempFile(t, "AAA\n"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	fs := &fileSync{base: 3, prefixSize: 4, prefixHasher: sha256.New()}
	fs.prefixHasher.Write([]byte("BBB")) // cache claims the first 3 bytes were "BBB"

	ok, err := c.verifyPrefix(f, fs, 3, 4, hexSHA("BBB"))
	if err != nil || !ok {
		t.Fatalf("fast path against cached digest: ok=%v err=%v", ok, err)
	}
	// The same call must reject a hash that matches the on-disk "AAA", because the
	// fast path never looks at the file.
	ok, err = c.verifyPrefix(f, fs, 3, 4, hexSHA("AAA"))
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("fast path should compare the cached digest, not re-read the file")
	}
}

// TestVerifyPrefixExtendsOverNewBytes proves the extend path hashes only the bytes
// the server gained, trusting the cache for the rest: after the historical bytes
// on disk are scribbled over, extending still matches because only the new span is
// read.
func TestVerifyPrefixExtendsOverNewBytes(t *testing.T) {
	c, _ := newTestClient(t)
	path := tempFile(t, "l1\nl2\n")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// Cold cache: a full hash of the first line populates the cache.
	fs := &fileSync{}
	if ok, err := c.verifyPrefix(f, fs, 3, 6, hexSHA("l1\n")); err != nil || !ok {
		t.Fatalf("cold verify: ok=%v err=%v", ok, err)
	}
	if fs.base != 3 {
		t.Fatalf("base after cold verify = %d, want 3", fs.base)
	}

	// Scribble over the already-cached bytes on disk. The extend path must not read
	// them: it hashes only [3,6) and appends to the cached digest of "l1\n".
	if err := os.WriteFile(path, []byte("XX\nl2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err := c.verifyPrefix(f, fs, 6, 6, hexSHA("l1\nl2\n"))
	if err != nil || !ok {
		t.Fatalf("extend verify: ok=%v err=%v (must trust cache for [0,3))", ok, err)
	}
	if fs.base != 6 {
		t.Fatalf("base after extend = %d, want 6", fs.base)
	}
}

// TestNextChunkRejectsOversizedOpenMessage checks the open region is capped before
// it is ever allocated: a Codex turn that never closes and grows past hardCap is
// refused rather than buffered whole.
func TestNextChunkRejectsOversizedOpenMessage(t *testing.T) {
	orig := hardCap
	hardCap = 16
	defer func() { hardCap = orig }()

	// Two complete lines, neither a user line, so the turn never closes; together
	// they exceed the shrunken cap.
	line := `{"type":"response_item","payload":{"role":"assistant"}}`
	content := line + "\n" + line + "\n"
	f, err := os.Open(tempFile(t, content))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if _, _, err := nextChunk(f, 0, 0, int64(len(content)), "codex", true); err == nil {
		t.Fatal("expected an oversized-message error for an open turn past hardCap")
	}
}

// TestRewindKeepsScanCursorBeforeFirstUpload covers the zero-byte-server case: a
// file that is only append-growing keeps its scan cursor (so a withheld opening
// message is not rescanned from zero every tick), while a file that diverged or
// shrank rescans from the start.
func TestRewindKeepsScanCursorBeforeFirstUpload(t *testing.T) {
	// Append-only growth, nothing uploaded yet: keep the cursor.
	fs := &fileSync{scanned: 100, prefixSize: 100}
	fs.rewind(150)
	if fs.scanned != 100 {
		t.Errorf("append-growing cursor = %d, want 100 (kept)", fs.scanned)
	}

	// Bytes had been uploaded (base > 0) and the server dropped to zero: the dropped
	// prefix holds boundaries, so rescan from the start.
	fs = &fileSync{base: 50, scanned: 80, prefixSize: 100}
	fs.rewind(150)
	if fs.scanned != 0 {
		t.Errorf("post-upload cursor = %d, want 0 (rescan)", fs.scanned)
	}

	// The file shrank: the scanned bytes are no longer what we saw, so rescan.
	fs = &fileSync{scanned: 80, prefixSize: 100}
	fs.rewind(50)
	if fs.scanned != 0 {
		t.Errorf("truncated cursor = %d, want 0 (rescan)", fs.scanned)
	}
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
