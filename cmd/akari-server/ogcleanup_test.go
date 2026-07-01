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
	if err := st.PutOverviewOGImage(ctx, fresh.ID, []byte("fresh-card")); err != nil {
		t.Fatal(err)
	}
	if err := st.PutOverviewOGImage(ctx, staleID, []byte("stale-card")); err != nil {
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
