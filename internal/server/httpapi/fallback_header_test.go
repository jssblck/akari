package httpapi

import (
	"context"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestSessionAuditLoadsFallbacks pins the ModelFallbackCount > 0 gate, which moved into
// the store's audit bundle so the tile's list, its count, and the transcript rows it
// annotates all come from one snapshot: a session whose rollup counted a fallback
// carries the capped fallback slice on its audit (and the windowed page carries the
// window's own rows), while a session with no fallback skips the read and leaves the
// slice empty. The header re-renders on every SSE refresh, so gating the read on the
// O(1) rollup keeps the common no-fallback case free.
func TestSessionAuditLoadsFallbacks(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := st.UpsertProject(ctx, "github.com/ada/hdr", "github.com", "ada", "hdr", "hdr", "remote")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{UserID: u.ID, Agent: "claude", SourceSessionID: "hdr-fb", ProjectID: pid})
	if err != nil {
		t.Fatal(err)
	}

	// Model the post-parse projection state: a message row, one fallback row hanging on
	// it, and the matching rollup.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO messages (session_id, ordinal, role, content) VALUES ($1, 1, 'assistant', 'served on the fallback model')`,
		ann.SessionID); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO model_fallbacks (session_id, message_ordinal, from_model, to_model, trigger, occurred_at, dedup_key)
		 VALUES ($1, 1, 'claude-fable-5', 'claude-opus-4-8', 'refusal', now(), 'req-hdr')`, ann.SessionID); err != nil {
		t.Fatalf("seed fallback: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET model_fallback_count = 1 WHERE id = $1", ann.SessionID); err != nil {
		t.Fatalf("stamp rollup: %v", err)
	}

	a, err := st.SessionAuditByID(ctx, ann.SessionID)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(a.Fallbacks) != 1 {
		t.Fatalf("audit fallbacks = %d, want 1 (the positive-count branch must load them)", len(a.Fallbacks))
	}
	if a.Fallbacks[0].FromModel != "claude-fable-5" || a.Fallbacks[0].ToModel != "claude-opus-4-8" {
		t.Errorf("loaded fallback models wrong: %+v", a.Fallbacks[0])
	}

	// The windowed page carries the same fallback beside the row it annotates, from the
	// window's own snapshot.
	snap, err := st.SessionSnapshotByID(ctx, ann.SessionID)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if len(snap.Page.Fallbacks) != 1 || snap.Page.Fallbacks[0].MessageOrdinal == nil || *snap.Page.Fallbacks[0].MessageOrdinal != 1 {
		t.Fatalf("window fallbacks = %+v, want the ordinal-1 notice", snap.Page.Fallbacks)
	}

	// A session with no fallback skips the read: the slice stays empty.
	bare, err := st.Announce(ctx, store.AnnounceParams{UserID: u.ID, Agent: "claude", SourceSessionID: "hdr-bare", ProjectID: pid})
	if err != nil {
		t.Fatal(err)
	}
	ba, err := st.SessionAuditByID(ctx, bare.SessionID)
	if err != nil {
		t.Fatalf("bare audit: %v", err)
	}
	if len(ba.Fallbacks) != 0 {
		t.Errorf("a no-fallback session should load no fallbacks, got %d", len(ba.Fallbacks))
	}
}
