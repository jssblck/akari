package store_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestPutBlobPinsAgainstSweep is the sweep-safety invariant for the client-CAS
// protocol: a body uploaded directly by the client, before any transcript
// references it, must survive the sweep. The pin protects it; only after the pin
// is forced to expire and the body is still unreferenced does the sweep reclaim it.
func TestPutBlobPinsAgainstSweep(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	body := []byte("a freshly uploaded tool result, not yet referenced")
	sha := store.HashBytes(body)

	if err := st.PutBlob(ctx, sha, "text/plain", "application/octet-stream", bytes.NewReader(body)); err != nil {
		t.Fatalf("put blob: %v", err)
	}

	// The body is in the CAS but no tool_call references it. A sweep must keep it,
	// because the pin is live.
	removed, err := st.SweepBlobs(ctx)
	if err != nil {
		t.Fatalf("sweep with live pin: %v", err)
	}
	if removed != 0 {
		t.Fatalf("sweep removed %d pinned, not-yet-referenced blob(s), want 0", removed)
	}
	if _, err := st.BlobMeta(ctx, sha); err != nil {
		t.Fatalf("pinned blob should survive the sweep: %v", err)
	}

	// Reading it back yields the exact bytes and media type the client uploaded.
	var buf bytes.Buffer
	media, err := st.WriteBlobTo(ctx, &buf, sha)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), body) || media != "text/plain" {
		t.Fatalf("read-back mismatch: %q (%s)", buf.Bytes(), media)
	}

	// Force the pin to expire; the still-unreferenced body is now reclaimable.
	if _, err := st.Pool.Exec(ctx, "UPDATE blob_pins SET expires_at = now() - interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	removed, err = st.SweepBlobs(ctx)
	if err != nil {
		t.Fatalf("sweep after pin expiry: %v", err)
	}
	if removed != 1 {
		t.Fatalf("sweep removed %d after pin expired, want 1", removed)
	}
	if _, err := st.BlobMeta(ctx, sha); err == nil {
		t.Fatal("expired, unreferenced blob should be gone after sweep")
	}
}

// TestSweepKeepsBlobWhoseExpiredPinIsMidRefresh guards the loss window the
// SKIP LOCKED reap opened: a pin sitting in the expired band that a refresher is
// extending right now must still protect its blob. The refresher's upsert takes the
// ON CONFLICT DO UPDATE path, which touches only expires_at (not the FK column), so
// it locks the pin row but takes no FOR KEY SHARE on the blobs row. The reap skips
// the locked pin, so if the orphan step trusts the committed (still-expired)
// expires_at it will classify the blob as unpinned and cascade it (and the pin the
// refresher is about to commit) away. Any pin row that survives the reap is either
// live or mid-refresh; both must keep the blob.
func TestSweepKeepsBlobWhoseExpiredPinIsMidRefresh(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	body := []byte("a body whose pin is being refreshed as the sweep runs")
	sha := store.HashBytes(body)
	if err := st.PutBlob(ctx, sha, "text/plain", "application/octet-stream", bytes.NewReader(body)); err != nil {
		t.Fatalf("put blob: %v", err)
	}
	// Drive the pin into the expired band the reap targets.
	if _, err := st.Pool.Exec(ctx, "UPDATE blob_pins SET expires_at = now() - interval '1 hour'"); err != nil {
		t.Fatal(err)
	}

	// A refresher extends the expired pin but has not committed. It holds the pin
	// row's write lock; because only expires_at changes, it holds no lock on the
	// blobs row, so nothing stops the sweep from reaching that blob.
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx,
		`INSERT INTO blob_pins (sha256, expires_at) VALUES ($1, now() + interval '1 hour')
		 ON CONFLICT (sha256) DO UPDATE SET expires_at = EXCLUDED.expires_at`, sha); err != nil {
		t.Fatalf("refresh pin: %v", err)
	}

	// Sweep while the refresh is in flight. With any surviving pin treated as
	// protective the sweep leaves the blob alone and returns promptly; the buggy
	// predicate read the committed expired expires_at, picked the blob as an orphan,
	// and blocked on the refresher's lock to cascade it away.
	type sweepResult struct {
		removed int
		err     error
	}
	done := make(chan sweepResult, 1)
	go func() {
		removed, err := st.SweepBlobs(ctx)
		done <- sweepResult{removed, err}
	}()

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("sweep: %v", res.err)
		}
		if res.removed != 0 {
			t.Fatalf("sweep removed %d blob(s) whose pin was mid-refresh, want 0", res.removed)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("sweep blocked on a mid-refresh pin: it tried to cascade away a blob a refresher was protecting")
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit pin refresh: %v", err)
	}
	if _, err := st.BlobMeta(ctx, sha); err != nil {
		t.Fatalf("blob whose pin was refreshed must survive the sweep: %v", err)
	}
}

// TestPutBlobPersistsContentType confirms PutBlob records the storage content type and
// BlobMeta reads it back, so the serve path can set Content-Encoding from it. An empty
// content type defaults to application/octet-stream (a raw body); a zstd label is kept
// verbatim, since the server stores the bytes opaquely and never inspects them.
func TestPutBlobPersistsContentType(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		body string
		ct   string
		want string
	}{
		{"explicit-raw", "a small raw body", "application/octet-stream", "application/octet-stream"},
		{"empty-defaults-to-raw", "a different body", "", "application/octet-stream"},
		{"zstd-preserved", "pretend these are compressed bytes", "application/zstd", "application/zstd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sha := store.HashString(tc.body) // distinct body per case, so no dedup collision
			if err := st.PutBlob(ctx, sha, "text/plain", tc.ct, strings.NewReader(tc.body)); err != nil {
				t.Fatalf("put blob: %v", err)
			}
			meta, err := st.BlobMeta(ctx, sha)
			if err != nil {
				t.Fatalf("blob meta: %v", err)
			}
			if meta.ContentType != tc.want {
				t.Errorf("content type = %q, want %q", meta.ContentType, tc.want)
			}
			if meta.MediaType != "text/plain" {
				t.Errorf("media type = %q, want text/plain", meta.MediaType)
			}
			if meta.ByteLen != int64(len(tc.body)) {
				t.Errorf("byte len = %d, want %d", meta.ByteLen, len(tc.body))
			}
		})
	}
}

// TestPutBlobRejectsHashMismatch confirms a body whose bytes do not match the
// declared hash is refused, so a corrupt upload cannot enter the CAS under a name
// a later transcript would serve.
func TestPutBlobRejectsHashMismatch(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	wrong := store.HashBytes([]byte("the real bytes"))
	if err := st.PutBlob(ctx, wrong, "text/plain", "application/octet-stream", strings.NewReader("different bytes")); err == nil {
		t.Fatal("expected a hash-mismatch error, got nil")
	}
	if _, err := st.BlobMeta(ctx, wrong); err == nil {
		t.Fatal("a mismatched blob must not be stored")
	}
}

// TestMissingBlobsReportsAbsentAndPinsPresent confirms the check endpoint's store
// method reports exactly the absent subset (so the client uploads only what the
// server lacks) and pins every present hash so it survives the sweep until the
// transcript that references it lands. The pin is the fix for the check-then-sweep
// race: a present, unreferenced body would otherwise be reclaimable between the
// check and the transcript append.
func TestMissingBlobsReportsAbsentAndPinsPresent(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	present := []byte("already stored")
	presentSHA := store.HashBytes(present)
	if err := st.PutBlob(ctx, presentSHA, "text/plain", "application/octet-stream", bytes.NewReader(present)); err != nil {
		t.Fatal(err)
	}
	// Force the upload pin to expire so only a fresh pin from the check could keep
	// the present blob alive.
	if _, err := st.Pool.Exec(ctx, "UPDATE blob_pins SET expires_at = now() - interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	absentSHA := store.HashBytes([]byte("never uploaded"))

	missing, err := st.MissingBlobs(ctx, []string{presentSHA, absentSHA})
	if err != nil {
		t.Fatalf("missing blobs: %v", err)
	}
	if len(missing) != 1 || missing[0] != absentSHA {
		t.Fatalf("missing = %v, want just the absent hash %s", missing, absentSHA)
	}

	// The present blob was re-pinned by the check, so a sweep keeps it even though it
	// is still unreferenced.
	if removed, err := st.SweepBlobs(ctx); err != nil || removed != 0 {
		t.Fatalf("sweep after check removed=%d err=%v, want 0 (check should re-pin)", removed, err)
	}
	if _, err := st.BlobMeta(ctx, presentSHA); err != nil {
		t.Fatalf("present blob should survive after the check re-pinned it: %v", err)
	}
}

// TestMissingBlobsEmpty confirms the no-candidates case returns an empty set
// without a query.
func TestMissingBlobsEmpty(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	missing, err := st.MissingBlobs(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want empty", missing)
	}
}

// TestApplyDeltaReferencesUploadedBlob confirms the parse path records a CAS
// reference (no blob write) when the client already uploaded the body, and that
// the reference plus the body survive a sweep together.
func TestApplyDeltaReferencesUploadedBlob(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	sid := seedSession(t, st, u.ID, projectID, "ref-sess")

	input := `{"file_path":"src/auth.ts"}`
	inputSHA := store.HashString(input)
	result := "export function login() {}"
	resultSHA := store.HashString(result)

	// The client uploaded both bodies before the transcript.
	if err := st.PutBlob(ctx, inputSHA, "application/json", "application/octet-stream", strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if err := st.PutBlob(ctx, resultSHA, "text/plain", "application/octet-stream", strings.NewReader(result)); err != nil {
		t.Fatal(err)
	}

	// The parsed delta carries references, not inline bodies.
	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "assistant", Content: "x", HasToolUse: true}},
		ToolCalls: []store.ProjToolCall{{
			MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", Category: "read",
			InputSHA256: inputSHA, InputBytes: int64(len(input)), InputMediaType: "application/json", CallUID: "c1",
		}},
		ToolResults: []store.ToolResultDelta{{
			CallUID: "c1", BodySHA256: resultSHA, Bytes: int64(len(result)), MediaType: "text/plain", Status: "ok",
		}},
	}
	rebuildWith(t, st, sid, delta)

	// The row carries the references with the declared metadata.
	var gotInput, gotResult string
	var inBytes, resBytes int64
	if err := st.Pool.QueryRow(ctx,
		`SELECT input_sha256, result_sha256, input_bytes, result_bytes FROM tool_calls WHERE session_id=$1`, sid).
		Scan(&gotInput, &gotResult, &inBytes, &resBytes); err != nil {
		t.Fatal(err)
	}
	if gotInput != inputSHA || gotResult != resultSHA {
		t.Fatalf("references: input=%s result=%s, want %s/%s", gotInput, gotResult, inputSHA, resultSHA)
	}
	if inBytes != int64(len(input)) || resBytes != int64(len(result)) {
		t.Fatalf("byte metadata: input=%d result=%d", inBytes, resBytes)
	}

	// Both blobs are now referenced, so the sweep keeps them even after pins expire.
	if _, err := st.Pool.Exec(ctx, "UPDATE blob_pins SET expires_at = now() - interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	if removed, err := st.SweepBlobs(ctx); err != nil || removed != 0 {
		t.Fatalf("sweep with live references removed=%d err=%v, want 0", removed, err)
	}
	if _, err := st.BlobMeta(ctx, inputSHA); err != nil {
		t.Fatalf("referenced input blob should survive: %v", err)
	}
}

// TestToolCallDetailRoundTrip confirms a tool call's detail (the bounded input
// summary the UI shows when a call has no file_path) is stored and read back on
// both projection paths: the inline body path (the server writes the blob) and the
// client-lifted sentinel path (the reference is recorded with the detail already
// derived off the sentinel), on both the unbounded ToolCalls read and the
// ordinal-windowed ToolCallsInRange a bounded transcript page uses. It is DB-gated
// and skips cleanly without a test database, like the other store tests.
func TestToolCallDetailRoundTrip(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "ada", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	sid := seedSession(t, st, u.ID, projectID, "detail-sess")

	// A client-lifted input: the blob is uploaded first, and the parsed delta carries
	// the reference plus the detail the client derived off the sentinel.
	liftedBody := `{"command":"go build ./..."}`
	liftedSHA := store.HashString(liftedBody)
	if err := st.PutBlob(ctx, liftedSHA, "application/json", "application/octet-stream", strings.NewReader(liftedBody)); err != nil {
		t.Fatal(err)
	}

	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "assistant", Content: "x", HasToolUse: true},
			{Ordinal: 1, Role: "assistant", Content: "y", HasToolUse: true},
		},
		ToolCalls: []store.ProjToolCall{
			{
				MessageOrdinal: 0, CallIndex: 0, ToolName: "Grep", Category: "search",
				Detail:         "func Reduce",
				InputBody:      `{"pattern":"func Reduce"}`,
				InputMediaType: "application/json", CallUID: "c-inline",
			},
			{
				MessageOrdinal: 0, CallIndex: 1, ToolName: "Bash", Category: "bash",
				Detail:      "go build ./...",
				InputSHA256: liftedSHA, InputBytes: int64(len(liftedBody)), InputMediaType: "application/json", CallUID: "c-sentinel",
			},
			// A third call on a later message, outside the window ToolCallsInRange
			// below asks for, so the test also pins that the range is exclusionary
			// and not just additive.
			{
				MessageOrdinal: 1, CallIndex: 0, ToolName: "Grep", Category: "search",
				Detail:         "func Winlock",
				InputBody:      `{"pattern":"func Winlock"}`,
				InputMediaType: "application/json", CallUID: "c-out-of-range",
			},
		},
	}
	rebuildWith(t, st, sid, delta)

	calls, err := st.ToolCalls(ctx, sid)
	if err != nil {
		t.Fatalf("read tool calls: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("read %d tool calls, want 3", len(calls))
	}
	if got, want := calls[0].Detail, "func Reduce"; got != want {
		t.Errorf("inline-path detail = %q, want %q", got, want)
	}
	if got, want := calls[1].Detail, "go build ./..."; got != want {
		t.Errorf("sentinel-path detail = %q, want %q", got, want)
	}
	// The sentinel-path call also carries its reference, so the detail rides beside
	// the CAS reference rather than an inline body.
	if calls[1].InputSHA != liftedSHA {
		t.Errorf("sentinel-path input sha = %q, want %q", calls[1].InputSHA, liftedSHA)
	}

	// ToolCallsInRange is the bounded-read sibling of ToolCalls (a transcript page
	// fetches only the calls for the messages it returned); it must carry Detail
	// through the same coalesce as the unbounded read, and the ordinal window must
	// exclude the call that hangs on message 1.
	inRange, err := st.ToolCallsInRange(ctx, sid, 0, 0)
	if err != nil {
		t.Fatalf("read tool calls in range: %v", err)
	}
	if len(inRange) != 2 {
		t.Fatalf("read %d tool calls in range [0,0], want 2 (the message-1 call must be excluded)", len(inRange))
	}
	if got, want := inRange[0].Detail, "func Reduce"; got != want {
		t.Errorf("ranged inline-path detail = %q, want %q", got, want)
	}
	if got, want := inRange[1].Detail, "go build ./..."; got != want {
		t.Errorf("ranged sentinel-path detail = %q, want %q", got, want)
	}
}

// TestApplyDeltaMissingUploadedBlobFails confirms a transcript referencing a body
// the CAS does not hold is refused rather than recording a dangling reference.
func TestApplyDeltaMissingUploadedBlobFails(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	sid := seedSession(t, st, u.ID, projectID, "missing-sess")

	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "assistant", Content: "x", HasToolUse: true}},
		ToolCalls: []store.ProjToolCall{{
			MessageOrdinal: 0, CallIndex: 0, ToolName: "Read",
			InputSHA256: store.HashString("never uploaded"), InputBytes: 5, InputMediaType: "application/json", CallUID: "c1",
		}},
	}
	err = st.RebuildSession(ctx, sid, testEpoch, stubReducer{delta})
	if !errors.Is(err, store.ErrBlobNotUploaded) {
		t.Fatalf("rebuild with a dangling blob reference = %v, want ErrBlobNotUploaded", err)
	}
}
