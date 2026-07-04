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

// TestGenerateProjectStoresRenderedCard drives GenerateProject against a real store: it
// queries the project-scoped analytics, renders, returns a valid PNG, and persists the
// same bytes the cache read then returns. It is the project mirror of
// TestGenerateStoresRenderedCard.
func TestGenerateProjectStoresRenderedCard(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PublishProjectOverview(ctx, projectID); err != nil {
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
		 VALUES ($1, 'claude-opus-4-8', 100, 50, 1.0, now() - make_interval(days => 1), 'p1')`,
		ann.SessionID); err != nil {
		t.Fatal(err)
	}

	returned, err := ogimage.GenerateProject(ctx, st, projectID, "github.com/jssblck/akari", time.Now())
	if err != nil {
		t.Fatalf("GenerateProject: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(returned))
	if err != nil {
		t.Fatalf("decode returned card: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != ogimage.Width || b.Dy() != ogimage.Height {
		t.Fatalf("card size = %dx%d, want %dx%d", b.Dx(), b.Dy(), ogimage.Width, ogimage.Height)
	}

	img, err := st.ProjectOGImage(ctx, projectID)
	if err != nil {
		t.Fatalf("load cached card: %v", err)
	}
	if !bytes.Equal(img.PNG, returned) {
		t.Fatal("cached project card bytes differ from the bytes GenerateProject returned")
	}
}

// TestGenerateProjectAbortsDuringReparse pins GenerateProject's epoch gate: while a
// session sits behind the running parser epoch, AnalyticsSnapshot reports not-ok, so
// GenerateProject returns ErrReparseInProgress with no PNG and writes nothing. Caching
// a card built from a half-rebuilt corpus is the failure this guards against.
func TestGenerateProjectAbortsDuringReparse(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	// A freshly announced session_raw row defaults to parser_epoch 0, so once the
	// store runs at epoch 1 it stands in for a session a rebuild has not yet reached.
	if _, err := st.Announce(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-1",
		ProjectID: projectID, Cwd: "/home/grace/akari", Machine: "laptop",
	}); err != nil {
		t.Fatal(err)
	}
	st.SetParserEpoch(1)

	pngBytes, err := ogimage.GenerateProject(ctx, st, projectID, "github.com/jssblck/akari", time.Now())
	if !errors.Is(err, ogimage.ErrReparseInProgress) {
		t.Fatalf("GenerateProject during reparse err = %v, want ErrReparseInProgress", err)
	}
	if pngBytes != nil {
		t.Fatalf("GenerateProject during reparse returned %d bytes, want nil", len(pngBytes))
	}
	if _, err := st.ProjectOGImage(ctx, projectID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cache after aborted render err = %v, want ErrNotFound (nothing stored)", err)
	}
}
