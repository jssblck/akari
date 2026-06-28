package store

import (
	"bytes"
	"context"
	"testing"
)

// seedSession announces a fresh session for the given user and returns its id.
func seedSession(t *testing.T, st *Store, userID, projectID int64, source string) int64 {
	t.Helper()
	ann, err := st.Announce(context.Background(), AnnounceParams{
		UserID: userID, Agent: "claude", SourceSessionID: source,
		ProjectID: projectID, GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce %s: %v", source, err)
	}
	return ann.SessionID
}

func TestCASWriteDedupReadSweep(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari")
	if err != nil {
		t.Fatal(err)
	}

	body := []byte(`{"file_path":"src/auth.ts"}`)
	bodySHA := HashBytes(body)

	// Two sessions whose tool calls share the same input body must dedupe to one
	// blob (content-addressed across sessions).
	s1 := seedSession(t, st, u.ID, projectID, "sess-1")
	s2 := seedSession(t, st, u.ID, projectID, "sess-2")
	proj := func(ord int) Projection {
		return Projection{
			ParserVersion: 1, MessageCount: 1,
			Messages: []ProjMessage{{Ordinal: 0, Role: "assistant", Content: "x", HasToolUse: true}},
			ToolCalls: []ProjToolCall{{
				MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", Category: "read",
				InputBody: body, InputBytes: int64(len(body)), InputMediaType: "application/json",
			}},
		}
	}
	if err := st.WriteProjection(ctx, s1, 0, proj(0)); err != nil {
		t.Fatalf("write projection s1: %v", err)
	}
	if err := st.WriteProjection(ctx, s2, 0, proj(0)); err != nil {
		t.Fatalf("write projection s2: %v", err)
	}

	var blobCount int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM blobs WHERE sha256 = $1", bodySHA).Scan(&blobCount); err != nil {
		t.Fatal(err)
	}
	if blobCount != 1 {
		t.Fatalf("blob rows for shared body = %d, want 1 (deduped)", blobCount)
	}

	// The body reads back byte-for-byte with its media type.
	var buf bytes.Buffer
	media, err := st.WriteBlobTo(ctx, &buf, bodySHA)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), body) {
		t.Fatalf("blob content = %q, want %q", buf.Bytes(), body)
	}
	if media != "application/json" {
		t.Fatalf("blob media = %q, want application/json", media)
	}

	// Both sessions reference the blob; an unrelated hash does not.
	for _, sid := range []int64{s1, s2} {
		ok, err := st.SessionReferencesBlob(ctx, sid, bodySHA)
		if err != nil || !ok {
			t.Fatalf("session %d should reference blob: ok=%v err=%v", sid, ok, err)
		}
	}
	if ok, _ := st.SessionReferencesBlob(ctx, s1, HashBytes([]byte("nope"))); ok {
		t.Fatal("session should not reference an unrelated hash")
	}

	// A sweep keeps the still-referenced blob.
	if removed, err := st.SweepBlobs(ctx); err != nil || removed != 0 {
		t.Fatalf("sweep with live refs removed=%d err=%v, want 0", removed, err)
	}

	// Re-projecting s1 and s2 with no tool calls orphans the blob; the sweep then
	// reclaims it.
	empty := Projection{ParserVersion: 1, MessageCount: 1,
		Messages: []ProjMessage{{Ordinal: 0, Role: "user", Content: "hi"}}}
	if err := st.WriteProjection(ctx, s1, 0, empty); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteProjection(ctx, s2, 0, empty); err != nil {
		t.Fatal(err)
	}
	removed, err := st.SweepBlobs(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 {
		t.Fatalf("sweep removed %d, want 1", removed)
	}
	if _, err := st.BlobMeta(ctx, bodySHA); err == nil {
		t.Fatal("blob should be gone after sweep")
	}
}

// TestSweepSkipsBlobLockedByWriter pins down the race fix: a blob a writer is
// re-referencing (holding FOR KEY SHARE in an open transaction) must not be
// reclaimed by a concurrent sweep, even though it is unreferenced in committed
// state.
func TestSweepSkipsBlobLockedByWriter(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari")
	if err != nil {
		t.Fatal(err)
	}

	body := []byte("shared tool body")
	sha := HashBytes(body)
	sid := seedSession(t, st, u.ID, projectID, "sess-1")
	withBlob := Projection{
		ParserVersion: 1, MessageCount: 1,
		Messages: []ProjMessage{{Ordinal: 0, Role: "assistant", Content: "x", HasToolUse: true}},
		ToolCalls: []ProjToolCall{{
			MessageOrdinal: 0, CallIndex: 0, ToolName: "Read",
			ResultBody: body, ResultBytes: int64(len(body)), ResultMediaType: "text/plain",
			ResultStatus: "ok", HasResult: true,
		}},
	}
	if err := st.WriteProjection(ctx, sid, 0, withBlob); err != nil {
		t.Fatal(err)
	}
	// Drop the reference in committed state: the blob is now an orphan a naive
	// sweep would remove.
	empty := Projection{ParserVersion: 1, MessageCount: 1,
		Messages: []ProjMessage{{Ordinal: 0, Role: "user", Content: "hi"}}}
	if err := st.WriteProjection(ctx, sid, 0, empty); err != nil {
		t.Fatal(err)
	}

	// A writer re-references the blob, holding FOR KEY SHARE in an open tx.
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := writeBlobTx(ctx, tx, body, "text/plain"); err != nil {
		t.Fatalf("writeBlobTx: %v", err)
	}

	// A concurrent sweep must skip the locked blob.
	removed, err := st.SweepBlobs(ctx)
	if err != nil {
		t.Fatalf("sweep while locked: %v", err)
	}
	if removed != 0 {
		t.Fatalf("sweep removed %d while a writer held the blob, want 0", removed)
	}
	if _, err := st.BlobMeta(ctx, sha); err != nil {
		t.Fatalf("blob should survive: %v", err)
	}

	// Once the writer is gone and the blob is truly orphaned, the sweep reclaims it.
	_ = tx.Rollback(ctx)
	removed, err = st.SweepBlobs(ctx)
	if err != nil {
		t.Fatalf("sweep after rollback: %v", err)
	}
	if removed != 1 {
		t.Fatalf("sweep removed %d after writer gone, want 1", removed)
	}
}
