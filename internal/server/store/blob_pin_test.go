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
	st := newTestStore(t)
	ctx := context.Background()

	body := []byte("a freshly uploaded tool result, not yet referenced")
	sha := HashBytes(body)

	if err := st.PutBlob(ctx, sha, "text/plain", bytes.NewReader(body)); err != nil {
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

// TestPutBlobRejectsHashMismatch confirms a body whose bytes do not match the
// declared hash is refused, so a corrupt upload cannot enter the CAS under a name
// a later transcript would serve.
func TestPutBlobRejectsHashMismatch(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	wrong := HashBytes([]byte("the real bytes"))
	if err := st.PutBlob(ctx, wrong, "text/plain", strings.NewReader("different bytes")); err == nil {
		t.Fatal("expected a hash-mismatch error, got nil")
	}
	if _, err := st.BlobMeta(ctx, wrong); err == nil {
		t.Fatal("a mismatched blob must not be stored")
	}
}

// TestHaveBlobsReportsPresence confirms the check endpoint's store method reports
// exactly the present subset, so the client uploads only what the server lacks.
func TestHaveBlobsReportsPresence(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	present := []byte("already stored")
	presentSHA := HashBytes(present)
	if err := st.PutBlob(ctx, presentSHA, "text/plain", bytes.NewReader(present)); err != nil {
		t.Fatal(err)
	}
	absentSHA := HashBytes([]byte("never uploaded"))

	have, err := st.HaveBlobs(ctx, []string{presentSHA, absentSHA})
	if err != nil {
		t.Fatalf("have blobs: %v", err)
	}
	if !have[presentSHA] {
		t.Errorf("present blob reported missing")
	}
	if have[absentSHA] {
		t.Errorf("absent blob reported present")
	}
}

// TestApplyDeltaReferencesUploadedBlob confirms the parse path records a CAS
// reference (no blob write) when the client already uploaded the body, and that
// the reference plus the body survive a sweep together.
func TestApplyDeltaReferencesUploadedBlob(t *testing.T) {
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
	if err := st.PutBlob(ctx, inputSHA, "application/json", strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
	if err := st.PutBlob(ctx, resultSHA, "text/plain", strings.NewReader(result)); err != nil {
		t.Fatal(err)
	}

	// The parsed delta carries references, not inline bodies.
	delta := ProjectionDelta{
		MessagesAdded: 1,
		Messages:      []MessageDelta{{Ordinal: 0, Role: "assistant", Content: "x", HasToolUse: true}},
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
		MessagesAdded: 1,
		Messages:      []MessageDelta{{Ordinal: 0, Role: "assistant", Content: "x", HasToolUse: true}},
		ToolCalls: []ProjToolCall{{
			MessageOrdinal: 0, CallIndex: 0, ToolName: "Read",
			InputSHA256: HashString("never uploaded"), InputBytes: 5, InputMediaType: "application/json", CallUID: "c1",
		}},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err == nil {
		t.Fatal("expected ErrBlobNotUploaded for a reference to an absent body")
	}
}
