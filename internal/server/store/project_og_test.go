package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestProjectOGImageLifecycle mirrors the overview card lifecycle for the per-project
// table: a fresh insert, a newer render replacing it in place, an older render skipped
// by the guarded upsert, and an equal-timestamp tie allowed to win.
func TestProjectOGImageLifecycle(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := st.ProjectOGImage(ctx, pid); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("read before store err = %v, want ErrNotFound", err)
	}

	t1 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	png := []byte("\x89PNG\r\n\x1a\n-fake-project-card")
	if wrote, err := st.PutProjectOGImage(ctx, pid, png, t1); err != nil || !wrote {
		t.Fatalf("first store = (wrote=%v, err=%v), want (true, nil)", wrote, err)
	}
	got, err := st.ProjectOGImage(ctx, pid)
	if err != nil {
		t.Fatalf("read after store: %v", err)
	}
	if string(got.PNG) != string(png) || !got.GeneratedAt.Equal(t1) {
		t.Fatalf("stored = (%q, %v), want (%q, %v)", got.PNG, got.GeneratedAt, png, t1)
	}

	// A newer render replaces the bytes; an older one is skipped by the guard.
	t2 := t1.Add(time.Hour)
	png2 := []byte("\x89PNG\r\n\x1a\n-refreshed-project")
	if wrote, err := st.PutProjectOGImage(ctx, pid, png2, t2); err != nil || !wrote {
		t.Fatalf("newer render = (wrote=%v, err=%v), want (true, nil)", wrote, err)
	}
	if wrote, err := st.PutProjectOGImage(ctx, pid, []byte("stale"), t1); err != nil || wrote {
		t.Fatalf("older render = (wrote=%v, err=%v), want (false, nil) (guarded update skipped)", wrote, err)
	}
	got2, err := st.ProjectOGImage(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	if string(got2.PNG) != string(png2) || !got2.GeneratedAt.Equal(t2) {
		t.Fatalf("after refresh+stale = (%q, %v), want the kept (%q, %v)", got2.PNG, got2.GeneratedAt, png2, t2)
	}
}

// TestPublicProjectCard pins the atomic read the /p/<id>/og.png serve depends on: the
// card comes back only while the project overview is public, so a card cannot be served
// for a private project even if one is cached. Three outcomes plus the after-unpublish
// close, matching TestPublicOverviewCard.
func TestPublicProjectCard(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}

	// Private: no row, found=false, the link 404s.
	if _, _, found, err := st.PublicProjectCard(ctx, pid); err != nil || found {
		t.Fatalf("private project card = (found=%v, err=%v), want (false, nil)", found, err)
	}

	// Public but uncached: found, with the project identity and no bytes.
	if err := st.PublishProjectOverview(ctx, pid); err != nil {
		t.Fatal(err)
	}
	gotProj, card, found, err := st.PublicProjectCard(ctx, pid)
	if err != nil || !found {
		t.Fatalf("public uncached = (found=%v, err=%v), want (true, nil)", found, err)
	}
	if gotProj.ID != pid || card.PNG != nil {
		t.Fatalf("public uncached = (id=%d, %d bytes), want (%d, 0 bytes)", gotProj.ID, len(card.PNG), pid)
	}

	// Public with a cached card: the bytes and stamp come back.
	stamp := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	png := []byte("\x89PNG\r\n\x1a\n-public-project-card")
	if _, err := st.PutProjectOGImage(ctx, pid, png, stamp); err != nil {
		t.Fatal(err)
	}
	_, card, found, err = st.PublicProjectCard(ctx, pid)
	if err != nil || !found {
		t.Fatalf("public cached = (found=%v, err=%v), want (true, nil)", found, err)
	}
	if string(card.PNG) != string(png) || !card.GeneratedAt.Equal(stamp) {
		t.Fatalf("public cached = (%q, %v), want (%q, %v)", card.PNG, card.GeneratedAt, png, stamp)
	}

	// Unpublishing closes the gate: the row survives, but the public read no longer
	// returns it, so a private project cannot serve its old card.
	if err := st.UnpublishProjectOverview(ctx, pid); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := st.PublicProjectCard(ctx, pid); err != nil || found {
		t.Fatalf("card after unpublish = (found=%v, err=%v), want (false, nil) despite the row still existing", found, err)
	}
	if _, err := st.ProjectOGImage(ctx, pid); err != nil {
		t.Fatalf("by-id read after unpublish: %v (the row should still exist, only the public gate closed)", err)
	}
}

// TestProjectOGImageStoreErrors pins the error paths: a backend failure surfaces as an
// error rather than a cache miss (read), a silent no-op (write/delete), or a clean
// "not public" (the atomic read). A cancelled context forces the failure deterministically.
func TestProjectOGImageStoreErrors(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := st.ProjectOGImage(cancelled, pid); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("read under cancelled ctx err = %v, want a non-ErrNotFound error", err)
	}
	if wrote, err := st.PutProjectOGImage(cancelled, pid, []byte("card"), time.Now()); err == nil || wrote {
		t.Fatalf("write under cancelled ctx = (wrote=%v, err=%v), want (false, error)", wrote, err)
	}
	if n, err := st.DeleteExpiredProjectOGImages(cancelled, time.Now()); err == nil || n != 0 {
		t.Fatalf("delete under cancelled ctx = (n=%d, err=%v), want (0, error)", n, err)
	}
	if _, _, found, err := st.PublicProjectCard(cancelled, pid); err == nil || found {
		t.Fatalf("public card read under cancelled ctx = (found=%v, err=%v), want (false, error)", found, err)
	}
}

// TestDeleteExpiredProjectOGImages pins the cleanup query: prune cards stamped before
// the cutoff, keep newer ones, return the count.
func TestDeleteExpiredProjectOGImages(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutProjectOGImage(ctx, pid, []byte("card"), time.Now()); err != nil {
		t.Fatal(err)
	}

	if n, err := st.DeleteExpiredProjectOGImages(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("prune fresh = (n=%d, err=%v), want (0, nil)", n, err)
	}
	if n, err := st.DeleteExpiredProjectOGImages(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune expired = (n=%d, err=%v), want (1, nil)", n, err)
	}
	if _, err := st.ProjectOGImage(ctx, pid); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired card after delete err = %v, want ErrNotFound", err)
	}
}
