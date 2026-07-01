package ogimage_test

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/ogimage"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestGenerateStoresRenderedCard drives Generate against a real store: it queries
// the user-scoped analytics, renders, returns a valid PNG, and persists the same
// bytes that the cache read then returns.
func TestGenerateStoresRenderedCard(t *testing.T) {
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
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-1",
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

	returned, err := ogimage.Generate(ctx, st, u, time.Now())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(returned))
	if err != nil {
		t.Fatalf("decode returned card: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != ogimage.Width || b.Dy() != ogimage.Height {
		t.Fatalf("card size = %dx%d, want %dx%d", b.Dx(), b.Dy(), ogimage.Width, ogimage.Height)
	}

	// The returned bytes are exactly what Generate cached.
	img, err := st.OverviewOGImage(ctx, u.ID)
	if err != nil {
		t.Fatalf("load cached card: %v", err)
	}
	if !bytes.Equal(img.PNG, returned) {
		t.Fatal("cached card bytes differ from the bytes Generate returned")
	}
}

// TestGenerateServesCanonicalOnLostRace pins the guarded-write reconciliation: when
// a newer card is already cached (a concurrent render with a later window won),
// Generate's own write is skipped, and it must return the canonical cached bytes, not
// the older card it just rendered. Otherwise two fetches of the same URL could unfurl
// different pictures: the loser's fresh render versus the winner's stored card.
func TestGenerateServesCanonicalOnLostRace(t *testing.T) {
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

	// Seed a card stamped in the future, standing in for a concurrent render whose
	// later window already won the cache. Generate below renders for an earlier now, so
	// its guarded write is skipped and it must fall back to these canonical bytes.
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	canonical := []byte("\x89PNG\r\n\x1a\n-canonical-winner")
	if wrote, err := st.PutOverviewOGImage(ctx, u.ID, canonical, now.Add(time.Hour)); err != nil || !wrote {
		t.Fatalf("seed canonical card: wrote=%v err=%v", wrote, err)
	}

	returned, err := ogimage.Generate(ctx, st, u, now)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !bytes.Equal(returned, canonical) {
		t.Fatalf("Generate returned its own render on a lost race, want the canonical cached bytes (%d bytes returned)", len(returned))
	}
	// And the cache is untouched: the newer card still stands.
	img, err := st.OverviewOGImage(ctx, u.ID)
	if err != nil {
		t.Fatalf("load cached card: %v", err)
	}
	if !bytes.Equal(img.PNG, canonical) {
		t.Fatal("a lost race overwrote the canonical cached card")
	}
}

// TestGenerateAbortsDuringReparse pins Generate's reparse gate directly: while a
// reparse holds the advisory lock, AnalyticsSnapshot reports not-ok, so Generate
// returns ErrReparseInProgress with no PNG and, crucially, writes nothing to the
// cache. Caching a card built from a half-rebuilt projection is the failure this
// guards against.
func TestGenerateAbortsDuringReparse(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}

	// Hold the reparse lock for the whole call, as a live reparse does for its run.
	lock, ok, err := st.AcquireReparseLock(ctx)
	if err != nil || !ok {
		t.Fatalf("acquire reparse lock: ok=%v err=%v", ok, err)
	}
	defer lock.Release(ctx)

	png, err := ogimage.Generate(ctx, st, u, time.Now())
	if !errors.Is(err, ogimage.ErrReparseInProgress) {
		t.Fatalf("Generate during reparse err = %v, want ErrReparseInProgress", err)
	}
	if png != nil {
		t.Fatalf("Generate during reparse returned %d bytes, want nil PNG", len(png))
	}
	// Nothing was cached: the aborted render must not leave a card behind.
	if _, err := st.OverviewOGImage(ctx, u.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cache after aborted render err = %v, want ErrNotFound (nothing stored)", err)
	}
}

// TestGenerateAnalyticsError pins Generate's analytics-failure path: when the
// underlying read errors, Generate returns no PNG and a wrapped error rather than
// caching a broken card. A cancelled context forces the read to fail deterministically.
func TestGenerateAnalyticsError(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	png, err := ogimage.Generate(cancelled, st, u, time.Now())
	if err == nil || errors.Is(err, ogimage.ErrReparseInProgress) {
		t.Fatalf("Generate with a failed analytics read err = %v, want a wrapped backend error", err)
	}
	if png != nil {
		t.Fatalf("Generate on analytics failure returned %d bytes, want nil PNG", len(png))
	}
}
