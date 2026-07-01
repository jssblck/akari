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

	// A published account with no card yet is a miss, both for serving and for the
	// refresh list (it has not published).
	if _, err := st.PublicOverviewOGImage(ctx, "grace"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("serve before publish err = %v, want ErrNotFound", err)
	}

	// Not published: the refresh list excludes it even though it has no card.
	stale, err := st.PublicOverviewsNeedingOGImage(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("unpublished account in refresh list: %+v", stale)
	}

	if err := st.PublishOverview(ctx, u.ID); err != nil {
		t.Fatal(err)
	}

	// Published but not rendered: it appears in the refresh list (card absent), yet
	// the public serve still 404s until a card exists.
	stale, err = st.PublicOverviewsNeedingOGImage(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].ID != u.ID || stale[0].Username != "grace" {
		t.Fatalf("refresh list after publish = %+v, want [grace]", stale)
	}
	if _, err := st.PublicOverviewOGImage(ctx, "grace"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("serve before render err = %v, want ErrNotFound", err)
	}

	// Store a card; the serve now returns the exact bytes.
	png := []byte("\x89PNG\r\n\x1a\n-fake-card-bytes")
	if err := st.PutOverviewOGImage(ctx, u.ID, png); err != nil {
		t.Fatal(err)
	}
	got, err := st.PublicOverviewOGImage(ctx, "grace")
	if err != nil {
		t.Fatalf("serve after render: %v", err)
	}
	if string(got.PNG) != string(png) {
		t.Fatalf("served bytes = %q, want %q", got.PNG, png)
	}
	if got.GeneratedAt.IsZero() {
		t.Fatal("generated_at not stamped")
	}

	// A fresh card is not stale: a cutoff in the past excludes it.
	stale, err = st.PublicOverviewsNeedingOGImage(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("fresh card flagged stale: %+v", stale)
	}
	// A cutoff in the future (nothing is new enough) includes it.
	stale, err = st.PublicOverviewsNeedingOGImage(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0].ID != u.ID {
		t.Fatalf("stale-by-age list = %+v, want [grace]", stale)
	}

	// A refresh replaces the bytes in place and re-stamps.
	png2 := []byte("\x89PNG\r\n\x1a\n-refreshed-card")
	if err := st.PutOverviewOGImage(ctx, u.ID, png2); err != nil {
		t.Fatal(err)
	}
	got2, err := st.PublicOverviewOGImage(ctx, "grace")
	if err != nil {
		t.Fatal(err)
	}
	if string(got2.PNG) != string(png2) {
		t.Fatalf("after refresh bytes = %q, want %q", got2.PNG, png2)
	}

	// Unpublishing hides the card from the public serve and from the refresh list,
	// without deleting the stored bytes (re-publishing brings the same card back).
	if err := st.UnpublishOverview(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PublicOverviewOGImage(ctx, "grace"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("serve after unpublish err = %v, want ErrNotFound", err)
	}
	stale, err = st.PublicOverviewsNeedingOGImage(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("unpublished account still in refresh list: %+v", stale)
	}
	if err := st.PublishOverview(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	got3, err := st.PublicOverviewOGImage(ctx, "grace")
	if err != nil {
		t.Fatalf("serve after re-publish: %v", err)
	}
	if string(got3.PNG) != string(png2) {
		t.Fatalf("re-published card = %q, want the kept %q", got3.PNG, png2)
	}
}
