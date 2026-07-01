package ogimage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// ErrReparseInProgress is returned by Generate when a reparse is (or was) running
// across its analytics read, so no card is stored. It is a skip, not a failure:
// callers log it quietly and let a later render (the post-reparse daily refresh)
// fill the card in once the projection is whole.
var ErrReparseInProgress = errors.New("ogimage: reparse in progress, skipping render")

// DefaultSince is the lower bound of the card's analytics window, measured back
// from now. It is deliberately identical to web.RangeSince(web.DefaultRange, now)
// — the public overview's default trailing-year window — so the card's figures
// reconcile exactly with the page a visitor lands on. The web view package is not
// imported here (the renderer stays free of the templ layer); the equality is
// pinned by a reconciliation test in the httpapi package, which imports both. The
// handler advertises the card only on that same default window, so a page shown
// under a narrower ?range never unfurls a mismatched year total.
func DefaultSince(now time.Time) time.Time { return now.AddDate(0, 0, -365) }

// DefaultUntil is the exclusive upper bound of the card's analytics window: the
// start of tomorrow (UTC), so the figures cover exactly the days the heatmap draws
// (the grid stops at today) and a future-dated event inflates neither. The public
// overview handler applies the same bound to its live analytics, so the page a card
// is advertised beside computes its totals over the identical window.
func DefaultUntil(now time.Time) time.Time {
	return now.UTC().Truncate(24*time.Hour).AddDate(0, 0, 1)
}

// Generate renders the preview card for one account's published overview and
// stores it. It queries the same windowed, user-scoped analytics the public page
// renders from, so the card's heatmap and figures match the page. It is the one
// path both the publish-time render (httpapi) and the daily refresh (the server's
// background loop) go through, so the two cannot drift.
//
// The analytics come from AnalyticsSnapshot, which reads them as a single
// consistent snapshot pinned so it cannot straddle a reparse: if a reparse is
// rewriting the projection when the snapshot is taken, it reports not-ok and
// Generate returns ErrReparseInProgress without storing anything, so a half-rebuilt
// aggregate is never cached. Coordinating in the store read (rather than a pre-check
// in each caller) covers both call sites by construction. A later render — the
// post-reparse daily refresh — fills the card once the projection is whole.
//
// The window is bounded on both sides: Since is the default trailing year, and
// Until is the end of the current UTC day, so the headline and caption cover exactly
// the days the heatmap draws (the grid stops at today) rather than folding a
// future-dated event into the total that no visible cell shows.
//
// now fixes the analytics window and the heatmap's trailing edge; the caller passes
// the wall clock (tests pass a fixed instant).
func Generate(ctx context.Context, st *store.Store, u store.User, now time.Time) error {
	a, ok, err := st.AnalyticsSnapshot(ctx, store.AnalyticsFilter{
		Since:   DefaultSince(now),
		Until:   DefaultUntil(now),
		UserIDs: []int64{u.ID},
	})
	if err != nil {
		return fmt.Errorf("loading overview analytics for user %d: %w", u.ID, err)
	}
	if !ok {
		return ErrReparseInProgress
	}

	png, err := Render(u.Username, a, now)
	if err != nil {
		return fmt.Errorf("rendering overview card for user %d: %w", u.ID, err)
	}
	if err := st.PutOverviewOGImage(ctx, u.ID, png); err != nil {
		return fmt.Errorf("storing overview card for user %d: %w", u.ID, err)
	}
	return nil
}
