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

	// Store a card stamped at t1; the read returns the exact bytes and that stamp.
	t1 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	png := []byte("\x89PNG\r\n\x1a\n-fake-card-bytes")
	if wrote, err := st.PutOverviewOGImage(ctx, u.ID, png, t1); err != nil {
		t.Fatal(err)
	} else if !wrote {
		t.Fatal("first store reported no write, want wrote=true (fresh insert)")
	}
	got, err := st.OverviewOGImage(ctx, u.ID)
	if err != nil {
		t.Fatalf("read after store: %v", err)
	}
	if string(got.PNG) != string(png) {
		t.Fatalf("stored bytes = %q, want %q", got.PNG, png)
	}
	if !got.GeneratedAt.Equal(t1) {
		t.Fatalf("generated_at = %v, want %v", got.GeneratedAt, t1)
	}

	// A newer render (t2 > t1) replaces the bytes in place (the one-per-user key) and
	// re-stamps.
	t2 := t1.Add(time.Hour)
	png2 := []byte("\x89PNG\r\n\x1a\n-refreshed-card")
	if wrote, err := st.PutOverviewOGImage(ctx, u.ID, png2, t2); err != nil {
		t.Fatal(err)
	} else if !wrote {
		t.Fatal("newer render reported no write, want wrote=true (guarded update fired)")
	}
	got2, err := st.OverviewOGImage(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got2.PNG) != string(png2) || !got2.GeneratedAt.Equal(t2) {
		t.Fatalf("after refresh = (%q, %v), want (%q, %v)", got2.PNG, got2.GeneratedAt, png2, t2)
	}

	// An older render (t1 < t2) must not clobber the newer card: the guarded upsert
	// leaves t2's card in place, so a slow render that read stale analytics cannot make
	// old content look fresh. This is the concurrency invariant PutOverviewOGImage
	// exists to hold.
	pngOld := []byte("\x89PNG\r\n\x1a\n-stale-loser")
	if wrote, err := st.PutOverviewOGImage(ctx, u.ID, pngOld, t1); err != nil {
		t.Fatal(err)
	} else if wrote {
		t.Fatal("older render reported a write, want wrote=false (guarded update skipped)")
	}
	got3, err := st.OverviewOGImage(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got3.PNG) != string(png2) || !got3.GeneratedAt.Equal(t2) {
		t.Fatalf("older render clobbered newer card: got (%q, %v), want the kept (%q, %v)", got3.PNG, got3.GeneratedAt, png2, t2)
	}

	// A render at the same instant (t2) is allowed to win: the render is deterministic
	// for a window, so an equal-timestamp overwrite is harmless.
	png4 := []byte("\x89PNG\r\n\x1a\n-tie-winner")
	if wrote, err := st.PutOverviewOGImage(ctx, u.ID, png4, t2); err != nil {
		t.Fatal(err)
	} else if !wrote {
		t.Fatal("equal-timestamp render reported no write, want wrote=true (>= guard admits ties)")
	}
	got4, err := st.OverviewOGImage(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(got4.PNG) != string(png4) {
		t.Fatalf("equal-timestamp render did not win: got %q, want %q", got4.PNG, png4)
	}
}

// TestPublicOverviewCard pins the atomic read the public og.png serve depends on:
// the card is returned only while the overview is public, so a card cannot be served
// for a private overview even if one is cached. It folds the public gate, the user
// lookup, and the card read into one query for exactly that reason. Three outcomes:
// private (found=false), public but uncached (found=true, no bytes), and public with
// a cached card (found=true, bytes).
func TestPublicOverviewCard(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}

	// Private overview: no row, so found is false and the link 404s.
	if _, _, found, err := st.PublicOverviewCard(ctx, "grace"); err != nil || found {
		t.Fatalf("private overview card = (found=%v, err=%v), want (false, nil)", found, err)
	}

	// Public but no card cached yet: found, with empty bytes so the caller renders.
	if err := st.PublishOverview(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	gotUser, card, found, err := st.PublicOverviewCard(ctx, "grace")
	if err != nil || !found {
		t.Fatalf("public uncached card = (found=%v, err=%v), want (true, nil)", found, err)
	}
	if gotUser.ID != u.ID || card.PNG != nil {
		t.Fatalf("public uncached = (userID=%d, %d card bytes), want (%d, 0 bytes)", gotUser.ID, len(card.PNG), u.ID)
	}

	// Public with a cached card: the bytes and stamp come back with the user.
	stamp := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	png := []byte("\x89PNG\r\n\x1a\n-public-card")
	if _, err := st.PutOverviewOGImage(ctx, u.ID, png, stamp); err != nil {
		t.Fatal(err)
	}
	_, card, found, err = st.PublicOverviewCard(ctx, "grace")
	if err != nil || !found {
		t.Fatalf("public cached card = (found=%v, err=%v), want (true, nil)", found, err)
	}
	if string(card.PNG) != string(png) || !card.GeneratedAt.Equal(stamp) {
		t.Fatalf("public cached card = (%q, %v), want (%q, %v)", card.PNG, card.GeneratedAt, png, stamp)
	}

	// Unpublishing closes the gate again: the cached row still exists, but the public
	// read no longer returns it, so an unpublished overview cannot serve its old card.
	if err := st.UnpublishOverview(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := st.PublicOverviewCard(ctx, "grace"); err != nil || found {
		t.Fatalf("card after unpublish = (found=%v, err=%v), want (false, nil) despite the row still existing", found, err)
	}
	if _, err := st.OverviewOGImage(ctx, u.ID); err != nil {
		t.Fatalf("by-id read after unpublish: %v (the row should still exist, only the public gate closed)", err)
	}
}

// TestOverviewOGImageStoreErrors pins the error paths of the three cache methods: a
// real backend failure must surface as an error, not be collapsed to ErrNotFound
// (reads) or a silent no-op (writes and deletes). A cancelled context forces a
// deterministic backend error without a broken database: pgx reports it before the
// query runs.
func TestOverviewOGImageStoreErrors(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	// A read failure is returned as itself, not misread as a cache miss: collapsing it
	// to ErrNotFound would make the handler render fresh on a transient backend blip and
	// hide the fault.
	if _, err := st.OverviewOGImage(cancelled, u.ID); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("read under a cancelled context err = %v, want a non-ErrNotFound error", err)
	}

	// A write failure reports wrote=false and the error, so a caller never treats a
	// failed store as a successful cache fill.
	if wrote, err := st.PutOverviewOGImage(cancelled, u.ID, []byte("card"), time.Now()); err == nil || wrote {
		t.Fatalf("write under a cancelled context = (wrote=%v, err=%v), want (false, error)", wrote, err)
	}

	// A delete failure reports zero removed and the error, so the cleanup loop logs it
	// rather than believing it pruned rows.
	if n, err := st.DeleteExpiredOGImages(cancelled, time.Now()); err == nil || n != 0 {
		t.Fatalf("delete under a cancelled context = (n=%d, err=%v), want (0, error)", n, err)
	}

	// The atomic public read surfaces a backend failure rather than reporting a clean
	// "not public": found is false (nothing to serve) but the error is returned, so the
	// handler 500s a broken lookup instead of 404ing a published overview.
	if _, _, found, err := st.PublicOverviewCard(cancelled, "grace"); err == nil || found {
		t.Fatalf("public card read under a cancelled context = (found=%v, err=%v), want (false, error)", found, err)
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
	if _, err := st.PutOverviewOGImage(ctx, u.ID, []byte("card"), time.Now()); err != nil {
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
