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

// cardHeading is the heading closure the tests pass GenerateSession, the same shape the
// handler builds: the card's project key stands in for web.ProjectLabel here so this package
// test stays free of the web layer.
func cardHeading(c store.SessionCard) string { return c.ProjectKey }

// generateSessionFixture registers an owner and a published session with a little usage,
// then returns its numeric id: GenerateSession reads every card input itself from that id.
func generateSessionFixture(t *testing.T, st *store.Store, ctx context.Context) int64 {
	t.Helper()
	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
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
		 VALUES ($1, 'claude-opus-4-8', 100, 50, 1.0, now() - make_interval(days => 1), 's1')`,
		ann.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PublishSession(ctx, ann.SessionID, u.ID, "pub-1"); err != nil {
		t.Fatal(err)
	}
	return ann.SessionID
}

// TestGenerateSessionStoresRenderedCard drives GenerateSession against a real store: it
// reads one session's card inputs in a snapshot, returns a valid PNG, and persists the same
// bytes the cache read then returns.
func TestGenerateSessionStoresRenderedCard(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := generateSessionFixture(t, st, ctx)

	returned, err := ogimage.GenerateSession(ctx, st, sid, cardHeading, time.Now())
	if err != nil {
		t.Fatalf("GenerateSession: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(returned))
	if err != nil {
		t.Fatalf("decode returned card: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != ogimage.Width || b.Dy() != ogimage.Height {
		t.Fatalf("card size = %dx%d, want %dx%d", b.Dx(), b.Dy(), ogimage.Width, ogimage.Height)
	}

	img, err := st.SessionOGImage(ctx, sid)
	if err != nil {
		t.Fatalf("load cached card: %v", err)
	}
	if !bytes.Equal(img.PNG, returned) {
		t.Fatal("cached session card bytes differ from the bytes GenerateSession returned")
	}
}

// TestGenerateSessionServesCanonicalOnLostRace pins the guarded-write reconciliation:
// when a newer card is already cached (a concurrent render stamped later won), the write
// is skipped and GenerateSession must return the canonical cached bytes, not its own
// render, so two fetches of the same URL cannot unfurl different pictures.
func TestGenerateSessionServesCanonicalOnLostRace(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := generateSessionFixture(t, st, ctx)

	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	canonical := []byte("\x89PNG\r\n\x1a\n-canonical-winner")
	if wrote, err := st.PutSessionOGImage(ctx, sid, canonical, now.Add(time.Hour)); err != nil || !wrote {
		t.Fatalf("seed canonical card: wrote=%v err=%v", wrote, err)
	}

	returned, err := ogimage.GenerateSession(ctx, st, sid, cardHeading, now)
	if err != nil {
		t.Fatalf("GenerateSession: %v", err)
	}
	if !bytes.Equal(returned, canonical) {
		t.Fatalf("GenerateSession returned its own render on a lost race, want the canonical cached bytes (%d bytes returned)", len(returned))
	}
	img, err := st.SessionOGImage(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(img.PNG, canonical) {
		t.Fatal("a lost race overwrote the canonical cached session card")
	}
}
