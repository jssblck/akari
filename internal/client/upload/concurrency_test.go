package upload

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// claudeToolResult builds a one-line Claude user message carrying a single tool_result
// whose content is lifted to the CAS, newline terminated. Distinct content yields a
// distinct body hash, so a sequence of these is a transcript with that many bodies.
func claudeToolResult(id, content string) string {
	return `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"` + id + `","content":` + jsonString(content) + `}]}}` + "\n"
}

// distinctBodySession builds a Claude transcript of n lines, each lifting a distinct
// body of about bodyLen bytes.
func distinctBodySession(n, bodyLen int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		content := fmt.Sprintf("body-%04d-", i) + strings.Repeat("x", bodyLen)
		b.WriteString(claudeToolResult(fmt.Sprintf("t%d", i), content))
	}
	return b.String()
}

// setMaxPendingBodyBytes temporarily shrinks the in-hand body budget so a test can
// exercise the early-flush path on small inputs.
func setMaxPendingBodyBytes(t *testing.T, n int64) {
	t.Helper()
	orig := maxPendingBodyBytes
	maxPendingBodyBytes = n
	t.Cleanup(func() { maxPendingBodyBytes = orig })
}

// setSettleWindow temporarily overrides the settle window so a test can force (or
// prevent) the flush of a withheld trailing turn.
func setSettleWindow(t *testing.T, d time.Duration) {
	t.Helper()
	orig := settleWindow
	settleWindow = d
	t.Cleanup(func() { settleWindow = orig })
}

// TestBatchedExistenceChecksBounded proves the client checks body presence in batched,
// parallel requests: every request carries at most blobCheckBatch hashes, the batches
// together cover every distinct body exactly once, and more than one runs concurrently.
func TestBatchedExistenceChecksBounded(t *testing.T) {
	setChunkTarget(t, 1<<30) // one chunk at finish, so all bodies flush together
	c, fs := newTestClient(t)
	fs.checkDelay = 5 * time.Millisecond // hold checks open long enough to overlap

	const n = 250
	content := distinctBodySession(n, 40)
	if _, err := c.SyncFile(context.Background(), target(tempFile(t, content))); err != nil {
		t.Fatal(err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.checkBatchSizes) < 3 {
		t.Fatalf("got %d check requests, want at least 3 for %d bodies at <=%d per request", len(fs.checkBatchSizes), n, blobCheckBatch)
	}
	total := 0
	for _, sz := range fs.checkBatchSizes {
		if sz > blobCheckBatch {
			t.Fatalf("a check request carried %d hashes, over the cap of %d", sz, blobCheckBatch)
		}
		total += sz
	}
	if total != n {
		t.Fatalf("check requests covered %d hashes in total, want exactly %d (one per distinct body)", total, n)
	}
	if fs.maxConcurrentChecks < 2 {
		t.Fatalf("peak concurrent checks = %d, want at least 2 (checks must run in parallel)", fs.maxConcurrentChecks)
	}
	if fs.puts != n {
		t.Fatalf("uploaded %d bodies, want %d", fs.puts, n)
	}
}

// TestDuplicateBodiesUploadOnce proves an identical body repeated across many lines is
// uploaded exactly once in a pass: the in-pass dedup collapses it before the existence
// check, so the CAS sees a single PUT.
func TestDuplicateBodiesUploadOnce(t *testing.T) {
	setChunkTarget(t, 1<<30)
	c, fs := newTestClient(t)

	var b strings.Builder
	for i := 0; i < 50; i++ {
		b.WriteString(claudeToolResult(fmt.Sprintf("t%d", i), "the very same body content repeated")) // identical content
	}
	if _, err := c.SyncFile(context.Background(), target(tempFile(t, b.String()))); err != nil {
		t.Fatal(err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.puts != 1 {
		t.Fatalf("uploaded the duplicate body %d times, want exactly 1", fs.puts)
	}
}

// TestEarlyFlushOnByteBudgetUploadsEverything proves the held-bytes budget forces an
// early flush (so memory stays bounded) without losing or double-uploading any body:
// with a tiny budget the pass flushes in several batches, and every distinct body still
// lands exactly once.
func TestEarlyFlushOnByteBudgetUploadsEverything(t *testing.T) {
	setChunkTarget(t, 1<<30)       // chunking would not flush on its own; force the byte budget to
	setMaxPendingBodyBytes(t, 512) // tiny budget: flush every few small bodies
	c, fs := newTestClient(t)

	const n = 60
	content := distinctBodySession(n, 64)
	if _, err := c.SyncFile(context.Background(), target(tempFile(t, content))); err != nil {
		t.Fatal(err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.puts != n {
		t.Fatalf("uploaded %d bodies, want %d", fs.puts, n)
	}
	if len(fs.checkBatchSizes) < 2 {
		t.Fatalf("got %d check requests, want several from the byte-budget early flush", len(fs.checkBatchSizes))
	}
	if string(fs.buf) == "" {
		t.Fatal("server stored no transcript, want the rewritten lines")
	}
}

// TestRegisterBodySkipsAlreadyPresent proves a body already confirmed present this pass
// is not re-queued, so it costs neither a check nor an upload. This is the present-skip
// branch, distinct from the duplicate-still-pending skip the dedup test covers.
func TestRegisterBodySkipsAlreadyPresent(t *testing.T) {
	c, _ := newTestClient(t)
	sink := &syncSink{c: c, present: newSeenCache(), pendingShas: map[string]struct{}{}, enc: c.enc}
	sha := hexSHA("a body already in the CAS")
	sink.present.add(sha)
	if err := sink.registerBody(context.Background(), sha, "application/octet-stream", bodyRef{haveContent: true, stored: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if len(sink.pending) != 0 {
		t.Fatalf("a present body was queued (%d pending), want it skipped", len(sink.pending))
	}
}

// TestCheckMissingFailurePropagates proves a failing existence check fails the sync
// rather than silently proceeding to upload or commit.
func TestCheckMissingFailurePropagates(t *testing.T) {
	setChunkTarget(t, 1<<30)
	c, fs := newTestClient(t)
	fs.failCheckStatus = http.StatusInternalServerError
	if _, err := c.SyncFile(context.Background(), target(tempFile(t, claudeToolResult("t1", "a body")))); err == nil {
		t.Fatal("expected an error when the blob-check endpoint fails")
	}
}

// TestPresentBodyIsNotReuploaded proves the existence check short-circuits the upload: a
// body a prior session already stored is reported present and never PUT again, even from
// a different session referencing it.
func TestPresentBodyIsNotReuploaded(t *testing.T) {
	setChunkTarget(t, 1<<30)
	c, fs := newTestClient(t)
	body := "a shared tool result body two sessions reference"

	if _, err := c.SyncFile(context.Background(), target(tempFile(t, claudeToolResult("t1", body)))); err != nil {
		t.Fatal(err)
	}
	if fs.puts != 1 {
		t.Fatalf("first sync uploaded %d bodies, want 1", fs.puts)
	}

	// A second session (fresh server-side session state, fresh client file state via a
	// new path) references the same body; the check reports it present, so no second PUT.
	fs.mu.Lock()
	fs.buf = nil
	fs.mu.Unlock()
	if _, err := c.SyncFile(context.Background(), target(tempFile(t, claudeToolResult("t2", body)))); err != nil {
		t.Fatal(err)
	}
	if fs.puts != 1 {
		t.Fatalf("present body re-uploaded: puts=%d, want 1", fs.puts)
	}
}

// TestPutBodyStatusErrorPropagates proves a failed upload carries the server's status
// code instead of being mistaken for a successful sync.
func TestPutBodyStatusErrorPropagates(t *testing.T) {
	setChunkTarget(t, 1<<30)
	c, fs := newTestClient(t)
	fs.failPutStatus = http.StatusServiceUnavailable
	_, err := c.SyncFile(context.Background(), target(tempFile(t, claudeToolResult("t1", "a body"))))
	if err == nil {
		t.Fatal("expected an error when the blob upload fails")
	}
	if !strings.Contains(err.Error(), "server returned 503") {
		t.Fatalf("error = %v, want server status %d", err, http.StatusServiceUnavailable)
	}
}

// TestWithheldCodexTurnBodiesUploadThisTick is the invariant that survives the move from
// inline to batched uploads: a body lifted from an open Codex trailing turn is uploaded
// the tick it is first transformed, even though the turn's transcript chunk is withheld
// until the turn closes. The held lines are cached and never re-transformed, so this is
// the body's only upload opportunity; if a later tick finally emits the chunk, the body
// must already be in the CAS.
func TestWithheldCodexTurnBodiesUploadThisTick(t *testing.T) {
	setSettleWindow(t, time.Hour) // keep the trailing turn open (not settled) on the first sync
	c, fs := newTestClient(t)

	codexTarget := func(path string) Target {
		return Target{Agent: "codex", Path: path, SourceID: "s1", ProjectKey: "github.com/o/r", Machine: "m"}
	}
	// An open turn: a function call (no liftable arguments) and its output, with no
	// closing user response_item, so the only lifted body is the output.
	body := "tool output " + strings.Repeat("z", 200)
	content := `{"type":"response_item","payload":{"type":"function_call"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"function_call_output","output":` + jsonString(body) + `}}` + "\n"
	path := tempFile(t, content)

	out, err := c.SyncFile(context.Background(), codexTarget(path))
	if err != nil {
		t.Fatal(err)
	}
	// The turn is withheld, so nothing is stored as transcript yet...
	if len(fs.buf) != 0 {
		t.Fatalf("server stored %q, want nothing while the turn is open", fs.buf)
	}
	if out.Action != ActionUpToDate {
		t.Fatalf("action = %s, want uptodate (no transcript stored)", out.Action)
	}
	// ...but the body it references is already uploaded and pinned.
	fs.mu.Lock()
	puts := fs.puts
	blobCount := len(fs.blobs)
	fs.mu.Unlock()
	if puts != 1 || blobCount != 1 {
		t.Fatalf("after open-turn sync: puts=%d blobs=%d, want the body uploaded once", puts, blobCount)
	}

	// Now let the turn settle and re-sync: the chunk flushes and references the body
	// without re-uploading it (the CAS already holds it).
	setSettleWindow(t, 0)
	if _, err := c.SyncFile(context.Background(), codexTarget(path)); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.puts != 1 {
		t.Fatalf("body uploaded %d times across both syncs, want exactly 1", fs.puts)
	}
	if len(fs.buf) == 0 {
		t.Fatal("after settle, server stored no transcript, want the withheld turn flushed")
	}
}

// TestFinalizeFlushesOpenCodexTurn proves Target.Finalize (`akari sync --finalize`)
// flushes a Codex session's trailing open turn on the first sync even though the file
// was just written and the idle settle window has not elapsed. Absent the flag that
// turn is withheld until the file goes idle (see TestWithheldCodexTurnBodiesUploadThisTick);
// on an ephemeral host (CI, a cloud sandbox) the window never elapses before teardown,
// so without --finalize the final turn would never upload.
func TestFinalizeFlushesOpenCodexTurn(t *testing.T) {
	setSettleWindow(t, time.Hour) // the file is fresh, so absent Finalize the turn stays withheld
	c, fs := newTestClient(t)

	// An open trailing turn: an assistant message with no closing user response_item, so
	// the client sees no turn boundary and would withhold it until the file settled.
	content := `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"text","text":"done"}]}}` + "\n"
	tgt := Target{Agent: "codex", Path: tempFile(t, content), SourceID: "s1", ProjectKey: "github.com/o/r", Machine: "m", Finalize: true}

	out, err := c.SyncFile(context.Background(), tgt)
	if err != nil {
		t.Fatal(err)
	}
	if out.Action != ActionUploaded {
		t.Fatalf("action = %s, want uploaded (finalize flushes the open turn immediately)", out.Action)
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.buf) == 0 {
		t.Fatal("server stored nothing; want the open trailing turn flushed by --finalize despite the fresh file")
	}
}
