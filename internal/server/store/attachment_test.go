package store_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestAttachmentProjectionWriteReadSweep exercises the inline attachment path end to
// end against the store: two inline attachments are written to the CAS and keyed by
// their content, both rows read back through Attachments with their media and size, a
// repeated rebuild from the same delta is idempotent, and the sweep keeps an
// attachment-only blob alive until the session is reset. The client-lifted reference
// path and the rebuild pin are covered by TestAttachmentReferencePathAndRebuildPin.
func TestAttachmentProjectionWriteReadSweep(t *testing.T) {
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

	// Both images arrive inline (the server decodes and stores them): one pasted by the
	// user, one generated on the assistant turn.
	generated := []byte("\x89PNG generated image bytes")
	generatedSHA := store.HashBytes(generated)
	pasted := []byte("\xff\xd8\xff jpeg pasted image bytes")
	pastedSHA := store.HashBytes(pasted)

	sid := seedSession(t, st, u.ID, projectID, "sess-1")

	// The pasted image, inline on the user turn, and the generated image, inline
	// (server writes it) on the assistant turn: both messages belong to the same
	// whole-session delta a rebuild folds together.
	delta := store.ProjectionDelta{
		Messages: []store.MessageDelta{
			{Ordinal: 0, Role: "user", Content: "trace this"},
			{Ordinal: 1, Role: "assistant", Content: "here you go", HasToolUse: true},
		},
		Attachments: []store.AttachmentDelta{
			{
				MessageOrdinal: 0, Body: string(pasted), Bytes: int64(len(pasted)),
				MediaType: "image/jpeg", Filename: "pasted.jpg",
			},
			{
				MessageOrdinal: 1, Body: string(generated), Bytes: int64(len(generated)),
				MediaType: "image/png", Filename: "kitten.png",
			},
		},
	}
	rebuildWith(t, st, sid, delta)

	// Both blobs exist and read back byte-for-byte under their image media types.
	for _, c := range []struct {
		sha   string
		bytes []byte
		media string
	}{
		{pastedSHA, pasted, "image/jpeg"},
		{generatedSHA, generated, "image/png"},
	} {
		var buf bytes.Buffer
		media, err := st.WriteBlobTo(ctx, &buf, c.sha)
		if err != nil {
			t.Fatalf("read attachment blob %s: %v", c.sha, err)
		}
		if !bytes.Equal(buf.Bytes(), c.bytes) {
			t.Fatalf("attachment blob %s content = %q, want %q", c.sha, buf.Bytes(), c.bytes)
		}
		if media != c.media {
			t.Fatalf("attachment blob %s media = %q, want %q", c.sha, media, c.media)
		}
	}

	// Attachments read back in message order, with media, size, and filename.
	atts, err := st.Attachments(ctx, sid)
	if err != nil {
		t.Fatalf("read attachments: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("read %d attachments, want 2", len(atts))
	}
	if atts[0].MessageOrdinal != 0 || atts[0].SHA256 != pastedSHA || atts[0].MediaType != "image/jpeg" ||
		atts[0].ByteLen != int64(len(pasted)) || atts[0].Filename != "pasted.jpg" {
		t.Errorf("pasted attachment view mismatch: %+v", atts[0])
	}
	if atts[1].MessageOrdinal != 1 || atts[1].SHA256 != generatedSHA || atts[1].MediaType != "image/png" ||
		atts[1].ByteLen != int64(len(generated)) || atts[1].Filename != "kitten.png" {
		t.Errorf("generated attachment view mismatch: %+v", atts[1])
	}

	// The session references both attachment blobs, and the sweep keeps them.
	for _, sha := range []string{pastedSHA, generatedSHA} {
		ok, err := st.SessionReferencesBlob(ctx, sid, sha)
		if err != nil || !ok {
			t.Fatalf("session should reference attachment blob %s: ok=%v err=%v", sha, ok, err)
		}
	}
	if removed, err := st.SweepBlobs(ctx); err != nil || removed != 0 {
		t.Fatalf("sweep with live attachments removed=%d err=%v, want 0", removed, err)
	}

	// Rebuilding again from the same delta is idempotent: a rebuild replaces the
	// whole projection, so re-running it lands the identical two rows rather than
	// duplicating or losing either attachment.
	rebuildWith(t, st, sid, delta)
	atts, err = st.Attachments(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 2 {
		t.Fatalf("after re-rebuild, read %d attachments, want 2 (idempotent)", len(atts))
	}

	// Resetting the session orphans both attachment blobs; the sweep reclaims them
	// once the rebuild pins lapse (each rebuild pins the prior projection's blobs
	// for the pin TTL, so expire the pins rather than waiting an hour).
	if err := st.ResetRaw(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx, "UPDATE blob_pins SET expires_at = now() - interval '1 hour'"); err != nil {
		t.Fatal(err)
	}
	removed, err := st.SweepBlobs(ctx)
	if err != nil {
		t.Fatalf("sweep after reset: %v", err)
	}
	if removed != 2 {
		t.Fatalf("sweep removed %d after reset, want 2", removed)
	}
}

// TestAttachmentReferencePathAndRebuildPin covers the client-lifted attachment path and
// the rebuild pin. A blob the client already uploaded is referenced by a sentinel
// (SHA256 set, no inline body), so the projection records the reference rather than
// writing bytes. A later rebuild whose delta drops the attachment (say the client
// removed it from the transcript) pins every blob the OLD projection referenced before
// deleting its rows, so a sweep racing between the pin and the new rows landing keeps
// the blob alive instead of reclaiming a still-recent image.
func TestAttachmentReferencePathAndRebuildPin(t *testing.T) {
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
	sid := seedSession(t, st, u.ID, projectID, "sess-ref")

	// The client uploaded the pasted image before the transcript that references it is
	// projected. Pre-store it through the same write the upload path takes, and key the
	// reference on the sha the store actually assigned (the hash of the stored bytes).
	pasted := []byte("\xff\xd8\xff jpeg client-lifted image bytes")
	tx, err := st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pastedSHA, err := store.WriteBlobTx(ctx, tx, string(pasted), "image/jpeg")
	if err != nil {
		tx.Rollback(ctx)
		t.Fatalf("pre-store referenced blob: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit referenced blob: %v", err)
	}

	// The sentinel reference: SHA256 set, no inline body, so the projection takes the
	// reference branch (a blobRef in resolveBlobsTx) rather than writing the bytes again.
	refDelta := store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "user", Content: "trace this"}},
		Attachments: []store.AttachmentDelta{{
			MessageOrdinal: 0, SHA256: pastedSHA, Bytes: int64(len(pasted)),
			MediaType: "image/jpeg", Filename: "pasted.jpg",
		}},
	}
	rebuildWith(t, st, sid, refDelta)

	// The reference reads back with the pre-stored blob's sha and the delta's metadata.
	atts, err := st.Attachments(ctx, sid)
	if err != nil {
		t.Fatalf("read attachments: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("read %d attachments, want 1", len(atts))
	}
	if atts[0].SHA256 != pastedSHA || atts[0].MediaType != "image/jpeg" ||
		atts[0].ByteLen != int64(len(pasted)) || atts[0].Filename != "pasted.jpg" {
		t.Errorf("referenced attachment view mismatch: %+v", atts[0])
	}

	// The reference keeps the blob alive: the session points at it, so a sweep skips it.
	if removed, err := st.SweepBlobs(ctx); err != nil || removed != 0 {
		t.Fatalf("sweep with a referenced attachment removed=%d err=%v, want 0", removed, err)
	}

	// A later rebuild's delta drops the attachment (the transcript no longer
	// references it). rebuildTx pins every blob the OLD projection referenced
	// before it deletes those rows, so even though the new projection carries no
	// reference to the image, the pin (not yet expired) keeps a concurrent sweep
	// from reclaiming it.
	rebuildWith(t, st, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "user", Content: "trace this"}},
	})
	atts, err = st.Attachments(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(atts) != 0 {
		t.Fatalf("rebuild left %d attachment rows, want 0", len(atts))
	}
	if removed, err := st.SweepBlobs(ctx); err != nil || removed != 0 {
		t.Fatalf("sweep right after the rebuild removed=%d err=%v, want 0 (blob is pinned)", removed, err)
	}
	if _, err := st.BlobMeta(ctx, pastedSHA); err != nil {
		t.Fatalf("rebuild-pinned blob should survive the sweep: %v", err)
	}
}
