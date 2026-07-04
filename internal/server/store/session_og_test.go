package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// publishedSession registers an owner, announces a session in a project, publishes it,
// and returns the numeric id and the minted public id. It is the fixture the session
// card tests build on.
func publishedSession(t *testing.T, st *store.Store, ctx context.Context, publicID string) (sessionID int64, mintedPublicID string, ownerID int64) {
	t.Helper()
	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-1",
		ProjectID: pid, Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.PublishSession(ctx, ann.SessionID, u.ID, publicID)
	if err != nil {
		t.Fatal(err)
	}
	return ann.SessionID, got, u.ID
}

// TestSessionOGImageLifecycle mirrors the overview card lifecycle for the per-session
// table: fresh insert, newer render wins, older render skipped by the guard.
func TestSessionOGImageLifecycle(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid, _, _ := publishedSession(t, st, ctx, "pub-1")

	if _, err := st.SessionOGImage(ctx, sid); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("read before store err = %v, want ErrNotFound", err)
	}

	t1 := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	png := []byte("\x89PNG\r\n\x1a\n-fake-session-card")
	if wrote, err := st.PutSessionOGImage(ctx, sid, png, t1); err != nil || !wrote {
		t.Fatalf("first store = (wrote=%v, err=%v), want (true, nil)", wrote, err)
	}
	got, err := st.SessionOGImage(ctx, sid)
	if err != nil {
		t.Fatalf("read after store: %v", err)
	}
	if string(got.PNG) != string(png) || !got.GeneratedAt.Equal(t1) {
		t.Fatalf("stored = (%q, %v), want (%q, %v)", got.PNG, got.GeneratedAt, png, t1)
	}

	t2 := t1.Add(time.Hour)
	png2 := []byte("\x89PNG\r\n\x1a\n-refreshed-session")
	if wrote, err := st.PutSessionOGImage(ctx, sid, png2, t2); err != nil || !wrote {
		t.Fatalf("newer render = (wrote=%v, err=%v), want (true, nil)", wrote, err)
	}
	if wrote, err := st.PutSessionOGImage(ctx, sid, []byte("stale"), t1); err != nil || wrote {
		t.Fatalf("older render = (wrote=%v, err=%v), want (false, nil) (guarded update skipped)", wrote, err)
	}
	got2, err := st.SessionOGImage(ctx, sid)
	if err != nil {
		t.Fatal(err)
	}
	if string(got2.PNG) != string(png2) || !got2.GeneratedAt.Equal(t2) {
		t.Fatalf("after refresh+stale = (%q, %v), want the kept (%q, %v)", got2.PNG, got2.GeneratedAt, png2, t2)
	}
}

// TestPublicSessionCard pins the atomic read the /s/<public_id>/og.png serve depends
// on: the card comes back only while the session is public. Uncached-public, cached,
// and after-unpublish close, matching TestPublicOverviewCard. An unknown public id is
// a clean not-found.
func TestPublicSessionCard(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid, pubID, ownerID := publishedSession(t, st, ctx, "pub-1")

	// Unknown public id: found=false, the link 404s.
	if _, _, found, err := st.PublicSessionCard(ctx, "nope"); err != nil || found {
		t.Fatalf("unknown session card = (found=%v, err=%v), want (false, nil)", found, err)
	}

	// Public but uncached: found, with the numeric id and no bytes.
	gotID, card, found, err := st.PublicSessionCard(ctx, pubID)
	if err != nil || !found {
		t.Fatalf("public uncached = (found=%v, err=%v), want (true, nil)", found, err)
	}
	if gotID != sid || card.PNG != nil {
		t.Fatalf("public uncached = (id=%d, %d bytes), want (%d, 0 bytes)", gotID, len(card.PNG), sid)
	}

	// Public with a cached card: the bytes and stamp come back.
	stamp := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	png := []byte("\x89PNG\r\n\x1a\n-public-session-card")
	if _, err := st.PutSessionOGImage(ctx, sid, png, stamp); err != nil {
		t.Fatal(err)
	}
	_, card, found, err = st.PublicSessionCard(ctx, pubID)
	if err != nil || !found {
		t.Fatalf("public cached = (found=%v, err=%v), want (true, nil)", found, err)
	}
	if string(card.PNG) != string(png) || !card.GeneratedAt.Equal(stamp) {
		t.Fatalf("public cached = (%q, %v), want (%q, %v)", card.PNG, card.GeneratedAt, png, stamp)
	}

	// Unpublishing clears the public id and gate: the row survives, but the public read
	// no longer resolves the (now-null) id, so a private session cannot serve its card.
	if err := st.UnpublishSession(ctx, sid, ownerID); err != nil {
		t.Fatal(err)
	}
	if _, _, found, err := st.PublicSessionCard(ctx, pubID); err != nil || found {
		t.Fatalf("card after unpublish = (found=%v, err=%v), want (false, nil) despite the row still existing", found, err)
	}
	if _, err := st.SessionOGImage(ctx, sid); err != nil {
		t.Fatalf("by-id read after unpublish: %v (the row should still exist, only the public gate closed)", err)
	}
}

// TestSessionOGImageStoreErrors pins the error paths: a backend failure surfaces rather
// than collapsing to a miss, a no-op, or a clean not-public. A cancelled context forces it.
func TestSessionOGImageStoreErrors(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid, pubID, _ := publishedSession(t, st, ctx, "pub-1")

	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	if _, err := st.SessionOGImage(cancelled, sid); err == nil || errors.Is(err, store.ErrNotFound) {
		t.Fatalf("read under cancelled ctx err = %v, want a non-ErrNotFound error", err)
	}
	if wrote, err := st.PutSessionOGImage(cancelled, sid, []byte("card"), time.Now()); err == nil || wrote {
		t.Fatalf("write under cancelled ctx = (wrote=%v, err=%v), want (false, error)", wrote, err)
	}
	if n, err := st.DeleteExpiredSessionOGImages(cancelled, time.Now()); err == nil || n != 0 {
		t.Fatalf("delete under cancelled ctx = (n=%d, err=%v), want (0, error)", n, err)
	}
	if _, _, found, err := st.PublicSessionCard(cancelled, pubID); err == nil || found {
		t.Fatalf("public card read under cancelled ctx = (found=%v, err=%v), want (false, error)", found, err)
	}
}

// TestSessionCard pins the one-snapshot read the session OG card renders from: the project
// identity for the heading, the title and rollup figures, the span, the gated grade, and the
// activity strip bucketed in SQL. Dated usage folds into buckets over the span (all four token
// classes summed), an undated event is excluded, and the histogram is exactly the requested
// length, so the strip stays bounded regardless of a session's event count.
func TestSessionCard(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)
	sid := seedSession(t, st, uid, pid, "card-session")

	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	// A first user message gives the card its title.
	rebuildWith(t, st, sid, store.ProjectionDelta{
		Messages: []store.MessageDelta{{Ordinal: 0, Role: "user", Content: "Ada asks about caching"}},
	})
	// The session rollups and span the card reads (the rebuild above set them from the delta;
	// this stamps the figures the card actually asserts against).
	if _, err := st.Pool.Exec(ctx,
		`UPDATE sessions SET message_count = 12,
		        total_input_tokens = 100, total_output_tokens = 20,
		        total_cache_read_tokens = 300, total_cache_write_tokens = 5,
		        started_at = $2, ended_at = $3 WHERE id = $1`,
		sid, start, end); err != nil {
		t.Fatalf("set session facts: %v", err)
	}
	insertScoredSignals(t, st, ctx, sid, 91, "A", false)
	// Dated usage across the span, plus an undated event the strip must exclude.
	seedUsageCacheAt(t, st, sid, "claude", 10, 2, 3, 1, start.Add(5*time.Minute), "e-early")
	seedUsageCacheAt(t, st, sid, "claude", 100, 20, 300, 5, start.Add(55*time.Minute), "e-late")
	seedUsageUndated(t, st, sid, "claude", 0, 7, 7, "e-undated")

	card, found, err := st.SessionCard(ctx, sid, 8)
	if err != nil || !found {
		t.Fatalf("session card = (found=%v, err=%v), want (true, nil)", found, err)
	}
	if card.ProjectKind != "remote" || card.ProjectKey != "github.com/jssblck/akari" {
		t.Fatalf("project identity = (%q, %q), want (remote, github.com/jssblck/akari)", card.ProjectKind, card.ProjectKey)
	}
	if card.Title != "Ada asks about caching" {
		t.Fatalf("title = %q, want %q", card.Title, "Ada asks about caching")
	}
	if card.MessageCount != 12 {
		t.Fatalf("message count = %d, want 12", card.MessageCount)
	}
	// TotalTokens folds all four rollup classes: 100+20+300+5 = 425.
	if card.TotalTokens != 425 {
		t.Fatalf("total tokens = %d, want 425", card.TotalTokens)
	}
	if card.Grade == nil || *card.Grade != "A" {
		t.Fatalf("grade = %v, want A", card.Grade)
	}
	if card.StartedAt == nil || card.EndedAt == nil {
		t.Fatalf("span not populated: %v..%v", card.StartedAt, card.EndedAt)
	}
	// The histogram is exactly the requested length, with the two dated events folded and the
	// undated one excluded: 16 (10+2+3+1) + 425 (100+20+300+5).
	if len(card.Activity) != 8 {
		t.Fatalf("activity buckets = %d, want 8", len(card.Activity))
	}
	var total int64
	nonzero := 0
	for _, v := range card.Activity {
		total += v
		if v > 0 {
			nonzero++
		}
	}
	if total != 16+425 {
		t.Fatalf("activity total = %d, want %d (two dated events; undated excluded)", total, 16+425)
	}
	// The early event (5 of 60 min) and the late one (55 of 60) fall in different buckets, so
	// the strip shows the work spread over time rather than piled in one cell.
	if nonzero != 2 {
		t.Fatalf("nonzero buckets = %d, want 2 (one per dated event)", nonzero)
	}
}

// TestSessionCardGatingAndEdges pins the gate and the no-span path: an unknown id is not
// found, a stale-flagged grade reads as unscored, and a session with no ended_at gets a nil
// activity strip even when it carries dated usage, so the card dashes a duration and draws an
// empty strip rather than inventing a span.
func TestSessionCardGatingAndEdges(t *testing.T) {
	t.Parallel()
	st, ctx, uid, pid := signalsEnv(t)

	if _, found, err := st.SessionCard(ctx, 999999, 8); err != nil || found {
		t.Fatalf("unknown session = (found=%v, err=%v), want (false, nil)", found, err)
	}

	sid := seedSession(t, st, uid, pid, "ungated")
	start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	if _, err := st.Pool.Exec(ctx, `UPDATE sessions SET started_at = $2 WHERE id = $1`, sid, start); err != nil {
		t.Fatalf("set started_at: %v", err)
	}
	insertScoredSignals(t, st, ctx, sid, 80, "B", true) // stale → gated out
	seedUsageCacheAt(t, st, sid, "claude", 10, 2, 3, 1, start.Add(time.Minute), "e1")

	card, found, err := st.SessionCard(ctx, sid, 8)
	if err != nil || !found {
		t.Fatalf("card = (found=%v, err=%v), want (true, nil)", found, err)
	}
	if card.Grade != nil {
		t.Fatalf("stale grade should read as unscored, got %v", *card.Grade)
	}
	if card.Activity != nil {
		t.Fatalf("no ended_at should yield a nil activity strip, got %d buckets", len(card.Activity))
	}
}

// TestDeleteExpiredSessionOGImages pins the cleanup query for the session table.
func TestDeleteExpiredSessionOGImages(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid, _, _ := publishedSession(t, st, ctx, "pub-1")

	if _, err := st.PutSessionOGImage(ctx, sid, []byte("card"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, err := st.DeleteExpiredSessionOGImages(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("prune fresh = (n=%d, err=%v), want (0, nil)", n, err)
	}
	if n, err := st.DeleteExpiredSessionOGImages(ctx, time.Now().Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("prune expired = (n=%d, err=%v), want (1, nil)", n, err)
	}
	if _, err := st.SessionOGImage(ctx, sid); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired card after delete err = %v, want ErrNotFound", err)
	}
}
