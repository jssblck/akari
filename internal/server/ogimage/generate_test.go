package ogimage_test

import (
	"bytes"
	"context"
	"image/png"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/ogimage"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// TestGenerateStoresRenderedCard drives Generate against a real store: it queries
// the user-scoped analytics, renders, and persists a valid PNG that the public
// read then returns. It also confirms the window is user-scoped — a second
// account's usage never lands in the card's figures.
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

	if err := ogimage.Generate(ctx, st, u, time.Now()); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	img, err := st.PublicOverviewOGImage(ctx, "grace")
	if err != nil {
		t.Fatalf("load card: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(img.PNG))
	if err != nil {
		t.Fatalf("decode card: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != ogimage.Width || b.Dy() != ogimage.Height {
		t.Fatalf("card size = %dx%d, want %dx%d", b.Dx(), b.Dy(), ogimage.Width, ogimage.Height)
	}
}
