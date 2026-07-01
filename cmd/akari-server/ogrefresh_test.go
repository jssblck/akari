package main

import (
	"context"
	"errors"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestRefreshStaleOGImages exercises the background refresh pass: it renders a
// card for a published overview that has none, is a no-op for an unpublished one,
// and re-renders a card once it ages past the staleness window.
func TestRefreshStaleOGImages(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	published, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PublishOverview(ctx, published.ID); err != nil {
		t.Fatal(err)
	}
	// A second account that never publishes: the refresh must not render it.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO users (username, password_hash, is_admin) VALUES ('ada', 'x', FALSE)`); err != nil {
		t.Fatal(err)
	}

	// Give grace some usage so the render has real figures to draw.
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: published.ID, Agent: "claude", SourceSessionID: "sess-1",
		ProjectID: projectID, Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1, 'claude-opus-4-8', 100, 50, 1.0, now() - make_interval(days => 1), 'u1')`,
		ann.SessionID); err != nil {
		t.Fatal(err)
	}

	// One pass renders grace's missing card and leaves ada (unpublished) alone.
	refreshStaleOGImages(ctx, st)

	first, err := st.PublicOverviewOGImage(ctx, "grace")
	if err != nil {
		t.Fatalf("grace card after refresh: %v", err)
	}
	if len(first.PNG) == 0 {
		t.Fatal("grace card is empty after refresh")
	}
	if _, err := st.PublicOverviewOGImage(ctx, "ada"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ada card after refresh err = %v, want ErrNotFound (never published)", err)
	}

	// A fresh card is not re-rendered on the next pass (it is not stale yet).
	refreshStaleOGImages(ctx, st)
	second, err := st.PublicOverviewOGImage(ctx, "grace")
	if err != nil {
		t.Fatal(err)
	}
	if !second.GeneratedAt.Equal(first.GeneratedAt) {
		t.Fatal("fresh card was needlessly re-rendered")
	}

	// Age the card past the staleness window; the next pass re-renders it.
	if _, err := st.Pool.Exec(ctx,
		`UPDATE overview_og_images SET generated_at = now() - make_interval(days => 2) WHERE user_id = $1`,
		published.ID); err != nil {
		t.Fatal(err)
	}
	refreshStaleOGImages(ctx, st)
	third, err := st.PublicOverviewOGImage(ctx, "grace")
	if err != nil {
		t.Fatal(err)
	}
	if !third.GeneratedAt.After(first.GeneratedAt) {
		t.Fatal("stale card was not re-rendered")
	}
}

// TestRefreshSkipsDuringReparse guards the reparse gate: while the projection is
// being rebuilt, the refresh pass must not render a card from a half-rebuilt
// aggregate. It holds the real reparse advisory lock (what a live reparse holds
// for its whole run), so the pass takes the same abort path a concurrent reparse
// would trigger. A published account with no card stays cardless until the lock
// clears.
func TestRefreshSkipsDuringReparse(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PublishOverview(ctx, u.ID); err != nil {
		t.Fatal(err)
	}

	// Hold the reparse advisory lock, standing in for a running reparse: the pass
	// must render nothing.
	lock, ok, err := st.AcquireReparseLock(ctx)
	if err != nil || !ok {
		t.Fatalf("acquire reparse lock: ok=%v err=%v", ok, err)
	}
	refreshStaleOGImages(ctx, st)
	if _, err := st.PublicOverviewOGImage(ctx, "grace"); !errors.Is(err, store.ErrNotFound) {
		lock.Release(ctx)
		t.Fatalf("card rendered during reparse: err = %v, want ErrNotFound", err)
	}

	// Once the lock clears, the next pass fills the card in.
	lock.Release(ctx)
	refreshStaleOGImages(ctx, st)
	if _, err := st.PublicOverviewOGImage(ctx, "grace"); err != nil {
		t.Fatalf("card not rendered after reparse cleared: %v", err)
	}
}
