package httpapi

import (
	"context"
	"testing"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestSessionHeaderStatsLoadsFallbacks pins the ModelFallbackCount > 0 branch of
// sessionHeaderStats: a session whose rollup counted a fallback loads the capped fallback
// slice into HeaderStats (for the header tile and the transcript notices), while a session
// with no fallback skips the read and leaves the slice empty. The header re-renders on every
// SSE refresh, so gating the read on the O(1) rollup keeps the common no-fallback case free.
func TestSessionHeaderStatsLoadsFallbacks(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	srv := New(st, config.Server{}, parse.NewWorker(st, 1, 0))

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

	// Model the post-parse projection state: one fallback row and the matching rollup.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO model_fallbacks (session_id, message_ordinal, from_model, to_model, trigger, occurred_at, dedup_key)
		 VALUES ($1, 1, 'claude-fable-5', 'claude-opus-4-8', 'refusal', now(), 'req-hdr')`, ann.SessionID); err != nil {
		t.Fatalf("seed fallback: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET model_fallback_count = 1 WHERE id = $1", ann.SessionID); err != nil {
		t.Fatalf("stamp rollup: %v", err)
	}

	d, err := st.SessionDetailByID(ctx, ann.SessionID)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	sig, err := st.SessionSignalsByID(ctx, d.ID)
	if err != nil {
		t.Fatalf("signals: %v", err)
	}
	hs, err := srv.sessionHeaderStats(ctx, d, sig)
	if err != nil {
		t.Fatalf("sessionHeaderStats: %v", err)
	}
	if len(hs.Fallbacks) != 1 {
		t.Fatalf("HeaderStats.Fallbacks = %d, want 1 (the positive-count branch must load them)", len(hs.Fallbacks))
	}
	if hs.Fallbacks[0].FromModel != "claude-fable-5" || hs.Fallbacks[0].ToModel != "claude-opus-4-8" {
		t.Errorf("loaded fallback models wrong: %+v", hs.Fallbacks[0])
	}

	// A session with no fallback skips the read: the slice stays empty.
	bare, err := st.Announce(ctx, store.AnnounceParams{UserID: u.ID, Agent: "claude", SourceSessionID: "hdr-bare", ProjectID: pid})
	if err != nil {
		t.Fatal(err)
	}
	bd, err := st.SessionDetailByID(ctx, bare.SessionID)
	if err != nil {
		t.Fatalf("bare detail: %v", err)
	}
	bsig, err := st.SessionSignalsByID(ctx, bd.ID)
	if err != nil {
		t.Fatalf("bare signals: %v", err)
	}
	bhs, err := srv.sessionHeaderStats(ctx, bd, bsig)
	if err != nil {
		t.Fatalf("bare sessionHeaderStats: %v", err)
	}
	if len(bhs.Fallbacks) != 0 {
		t.Errorf("a no-fallback session should load no fallbacks, got %d", len(bhs.Fallbacks))
	}
}
