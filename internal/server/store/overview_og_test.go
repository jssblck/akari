package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

func TestOverviewOGImageLifecycle(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}

	// No card cached yet: the by-id read is a miss.
	if _, err := st.OverviewOGImage(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("read before store err = %v, want ErrNotFound", err)
	}

	// Store a card; the read now returns the exact bytes and a stamp.
	png := []byte("\x89PNG\r\n\x1a\n-fake-card-bytes")
	if err := st.PutOverviewOGImage(ctx, u.ID, png); err != nil {
		t.Fatal(err)
	}
	got, err := st.OverviewOGImage(ctx, u.ID)
	if err != nil {
		t.Fatalf("read after store: %v", err)
	}
	if string(got.PNG) != string(png) {
		t.Fatalf("stored bytes = %q, want %q", got.PNG, png)
	}
	if got.GeneratedAt.IsZero() {
		t.Fatal("generated_at not stamped")
	}

	// A refresh replaces the bytes in place (the one-per-user key) and re-stamps.
	png2 := []byte("\x89PNG\r\n\x1a\n-refreshed-card")
	if err := st.PutOverviewOGImage(ctx, u.ID, png2); err != nil {
		t.Fatal(err)
	}
	got2, err := st.OverviewOGImage(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got2.PNG) != string(png2) {
		t.Fatalf("after refresh bytes = %q, want %q", got2.PNG, png2)
	}
}

// TestDeleteExpiredOGImages pins the cleanup query: it prunes cards stamped before
// the cutoff and leaves newer ones, returning the count removed.
func TestDeleteExpiredOGImages(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutOverviewOGImage(ctx, u.ID, []byte("card")); err != nil {
		t.Fatal(err)
	}

	// A cutoff before the card's stamp removes nothing.
	n, err := st.DeleteExpiredOGImages(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("deleted %d fresh card(s), want 0", n)
	}
	if _, err := st.OverviewOGImage(ctx, u.ID); err != nil {
		t.Fatalf("fresh card was deleted: %v", err)
	}

	// A cutoff after the stamp (nothing is new enough) removes it.
	n, err = st.DeleteExpiredOGImages(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("deleted %d expired card(s), want 1", n)
	}
	if _, err := st.OverviewOGImage(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired card after delete err = %v, want ErrNotFound", err)
	}
}
