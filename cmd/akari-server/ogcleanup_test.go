package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestCleanupExpiredOGImages exercises the background cleanup pass: it prunes a
// cached card older than the TTL and leaves a fresh one in place, so a card for an
// overview nobody is sharing does not linger while an actively shared one survives.
func TestCleanupExpiredOGImages(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	fresh, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	// A second account, inserted directly (registration past the first user is
	// invite-gated, which this test does not need).
	var staleID int64
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO users (username, password_hash, is_admin) VALUES ('ada', 'x', FALSE) RETURNING id`).Scan(&staleID); err != nil {
		t.Fatal(err)
	}

	// Both accounts hold a cached card; ada's is aged past the TTL.
	if _, err := st.PutOverviewOGImage(ctx, fresh.ID, []byte("fresh-card"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutOverviewOGImage(ctx, staleID, []byte("stale-card"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool.Exec(ctx,
		`UPDATE overview_og_images SET generated_at = now() - make_interval(hours => 2) WHERE user_id = $1`,
		staleID); err != nil {
		t.Fatal(err)
	}

	// One pass with a 1h TTL prunes ada's aged card and keeps grace's fresh one.
	cleanupExpiredOGImages(ctx, st, time.Hour)

	if _, err := st.OverviewOGImage(ctx, staleID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("stale card after cleanup err = %v, want ErrNotFound (pruned)", err)
	}
	if _, err := st.OverviewOGImage(ctx, fresh.ID); err != nil {
		t.Fatalf("fresh card after cleanup: %v (should have survived)", err)
	}
}

// TestCleanupExpiredOGImagesSwallowsError pins the pass's error branch: a failing
// delete is logged and swallowed, not propagated, so one bad sweep cannot terminate
// the background loop. A cancelled context forces the delete to fail deterministically;
// the cached card is left untouched.
func TestCleanupExpiredOGImagesSwallowsError(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutOverviewOGImage(ctx, u.ID, []byte("card"), time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	// Must not panic despite the delete failing; the aged card is still present because
	// nothing was pruned.
	cleanupExpiredOGImages(cancelled, st, time.Hour)

	if _, err := st.OverviewOGImage(ctx, u.ID); err != nil {
		t.Fatalf("card after a failed cleanup pass: %v (should be untouched)", err)
	}
}

// TestRunOGCleanupLoop exercises the loop itself, not just one pass: with a short
// interval it prunes an expired card on a tick without any request driving it, and it
// returns promptly once its context is cancelled (so shutdown does not hang on it).
func TestRunOGCleanupLoop(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx, cancel := context.WithCancel(context.Background())

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	// A card already aged past the TTL when the loop starts, so the first tick prunes
	// it.
	if _, err := st.PutOverviewOGImage(ctx, u.ID, []byte("expired-card"), time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		runOGCleanup(ctx, st, 10*time.Millisecond, time.Hour)
		close(done)
	}()

	// A tick fires and prunes the aged card; poll until it is gone rather than pinning
	// a single sleep to the ticker period.
	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := st.OverviewOGImage(ctx, u.ID)
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if err != nil {
			cancel()
			t.Fatalf("poll cached card: %v", err)
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("cleanup loop did not prune the expired card within 2s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Cancelling the context stops the loop; it must return rather than spin.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup loop did not return after context cancel")
	}
}
