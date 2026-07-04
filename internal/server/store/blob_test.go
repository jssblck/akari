package store_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// seedSession announces a fresh session for the given user and returns its id.
func seedSession(t *testing.T, st *store.Store, userID, projectID int64, source string) int64 {
	t.Helper()
	ann, err := st.Announce(context.Background(), store.AnnounceParams{
		UserID: userID, Agent: "claude", SourceSessionID: source,
		ProjectID: projectID, GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce %s: %v", source, err)
	}
	return ann.SessionID
}

func TestCASWriteDedupReadSweep(t *testing.T) {
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

	body := []byte(`{"file_path":"src/auth.ts"}`)
	bodySHA := store.HashBytes(body)

	// Two sessions whose tool calls share the same input body must dedupe to one
	// blob (content-addressed across sessions).
	s1 := seedSession(t, st, u.ID, projectID, "sess-1")
	s2 := seedSession(t, st, u.ID, projectID, "sess-2")
	withInput := store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "assistant", Content: "x", HasToolUse: true}},
		ToolCalls: []store.ProjToolCall{{
			MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", Category: "read",
			InputBody: string(body), InputBytes: int64(len(body)), InputMediaType: "application/json", CallUID: "c1",
		}},
	}
	rebuildWith(t, st, s1, withInput)
	rebuildWith(t, st, s2, withInput)

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
	if ok, _ := st.SessionReferencesBlob(ctx, s1, store.HashBytes([]byte("nope"))); ok {
		t.Fatal("session should not reference an unrelated hash")
	}

	// A sweep keeps the still-referenced blob.
	if removed, err := st.SweepBlobs(ctx); err != nil || removed != 0 {
		t.Fatalf("sweep with live refs removed=%d err=%v, want 0", removed, err)
	}

	// Resetting both sessions drops their tool calls, orphaning the blob; the
	// sweep then reclaims it.
	if err := st.ResetRaw(ctx, s1); err != nil {
		t.Fatal(err)
	}
	if err := st.ResetRaw(ctx, s2); err != nil {
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

	body := []byte("shared tool body")
	sha := store.HashBytes(body)
	sid := seedSession(t, st, u.ID, projectID, "sess-1")
	withBlob := store.ProjectionDelta{
		Messages:  []store.MessageDelta{{Ordinal: 0, Role: "assistant", Content: "x", HasToolUse: true}},
		ToolCalls: []store.ProjToolCall{{MessageOrdinal: 0, CallIndex: 0, ToolName: "Read", CallUID: "c1"}},
		ToolResults: []store.ToolResultDelta{{
			CallUID: "c1", Body: string(body), Bytes: int64(len(body)), MediaType: "text/plain", Status: "ok",
		}},
	}
	rebuildWith(t, st, sid, withBlob)
	// Drop the reference in committed state: the blob is now an orphan a naive
	// sweep would remove.
	if err := st.ResetRaw(ctx, sid); err != nil {
		t.Fatal(err)
	}

	// A writer re-references the blob, holding FOR KEY SHARE in an open tx.
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := store.WriteBlobTx(ctx, tx, string(body), "text/plain"); err != nil {
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
