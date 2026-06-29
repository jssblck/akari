package store

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestPutBlobPinsAgainstSweep is the sweep-safety invariant for the client-CAS
// protocol: a body uploaded directly by the client, before any transcript
// references it, must survive the sweep. The pin protects it; only after the pin
// is forced to expire and the body is still unreferenced does the sweep reclaim it.
func TestPutBlobPinsAgainstSweep(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	ctx := context.Background()

	body := []byte("a freshly uploaded tool result, not yet referenced")
	sha := HashBytes(body)

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

// TestPutBlobPersistsContentType confirms PutBlob records the storage content type and
// BlobMeta reads it back, so the serve path can set Content-Encoding from it. An empty
// content type defaults to application/octet-stream (a raw body); a zstd label is kept
// verbatim, since the server stores the bytes opaquely and never inspects them.
func TestPutBlobPersistsContentType(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
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
			sha := HashString(tc.body) // distinct body per case, so no dedup collision
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
	st := newTestStore(t)
	ctx := context.Background()

	wrong := HashBytes([]byte("the real bytes"))
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
	st := newTestStore(t)
	ctx := context.Background()

	present := []byte("already stored")
	presentSHA := HashBytes(present)
	if err := st.PutBlob(ctx, presentSHA, "text/plain", "application/octet-stream", bytes.NewReader(present)); err != nil {
		t.Fatal(err)
	}
	// Force the upload pin to expire so only a fresh pin from the check could keep
	// the present blob alive.
	if _, err := st.Pool.Exec(ctx, "UPDATE blob_pins SET expires_at = now() - interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	absentSHA := HashBytes([]byte("never uploaded"))

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
	st := newTestStore(t)
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
	st := newTestStore(t)
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
	inputSHA := HashString(input)
	result := "export function login() {}"
	resultSHA := HashString(result)

	// The client uploaded both bodies before the transcript.
	if err := st.PutBlob(ctx, inputSHA, "application/json", "application/octet-stream", strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if err := st.PutBlob(ctx, resultSHA, "text/plain", "application/octet-stream", strings.NewReader(result)); err != nil {
		t.Fatal(err)
	}

	// The parsed delta carries references, not inline bodies.
	delta := ProjectionDelta{
		Messages: []MessageDelta{{Ordinal: 0, Role: "assistant", Content: "x", HasToolUse: true}},
		ToolCalls: []ProjToolCall{{
			MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", Category: "read",
			InputSHA256: inputSHA, InputBytes: int64(len(input)), InputMediaType: "application/json", CallUID: "c1",
		}},
		ToolResults: []ToolResultDelta{{
			CallUID: "c1", BodySHA256: resultSHA, Bytes: int64(len(result)), MediaType: "text/plain", Status: "ok",
		}},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply projection: %v", err)
	}

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

// TestApplyDeltaMissingUploadedBlobFails confirms a transcript referencing a body
// the CAS does not hold is refused rather than recording a dangling reference.
func TestApplyDeltaMissingUploadedBlobFails(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
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

	delta := ProjectionDelta{
		Messages: []MessageDelta{{Ordinal: 0, Role: "assistant", Content: "x", HasToolUse: true}},
		ToolCalls: []ProjToolCall{{
			MessageOrdinal: 0, CallIndex: 0, ToolName: "Read",
			InputSHA256: HashString("never uploaded"), InputBytes: 5, InputMediaType: "application/json", CallUID: "c1",
		}},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err == nil {
		t.Fatal("expected ErrBlobNotUploaded for a reference to an absent body")
	}
}
