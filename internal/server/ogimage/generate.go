package ogimage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// ErrReparseInProgress is returned by Generate when a reparse is (or was) running
// across its analytics read, so no card is stored. It is a skip, not a failure: the
// image handler serves the last good card if it holds one, else 404s, and a fetch
// after the reparse finishes renders the card once the projection is whole.
var ErrReparseInProgress = errors.New("ogimage: reparse in progress, skipping render")

// DefaultSince is the lower bound of the card's analytics window, measured back
// from now. It is deliberately identical to web.RangeSince(web.DefaultRange, now)
// (the public overview's default trailing-year window), so the card's figures
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

// Generate renders the preview card for one account's published overview, stores it
// in the cache, and returns the encoded PNG so the caller can serve the very bytes
// it just rendered without a second read. It queries the same windowed, user-scoped
// analytics the public page renders from, so the card's heatmap and figures match
// the page. It is the one render+store path the on-demand image handler goes through
// (on a cache miss or a stale card), so a cached card and a freshly served one
// cannot drift.
//
// The analytics come from AnalyticsSnapshot, which reads them as a single
// consistent snapshot pinned so it cannot straddle a reparse: if a reparse is
// rewriting the projection when the snapshot is taken, it reports not-ok and
// Generate returns ErrReparseInProgress without storing anything, so a half-rebuilt
// aggregate is never cached. Coordinating in the store read (rather than a pre-check
// in the caller) keeps the on-demand path correct by construction. The image handler
// then serves the last good card if it holds one, and a later fetch renders the card
// once the projection is whole.
//
// The window is bounded on both sides: Since is the default trailing year, and
// Until is the end of the current UTC day, so the headline and caption cover exactly
// the days the heatmap draws (the grid stops at today) rather than folding a
// future-dated event into the total that no visible cell shows.
//
// now fixes the analytics window and the heatmap's trailing edge; the caller passes
// the wall clock (tests pass a fixed instant).
func Generate(ctx context.Context, st *store.Store, u store.User, now time.Time) ([]byte, error) {
	a, ok, err := st.AnalyticsSnapshot(ctx, store.AnalyticsFilter{
		Since:   DefaultSince(now),
		Until:   DefaultUntil(now),
		UserIDs: []int64{u.ID},
	})
	if err != nil {
		return nil, fmt.Errorf("loading overview analytics for user %d: %w", u.ID, err)
	}
	if !ok {
		return nil, ErrReparseInProgress
	}

	png, err := Render(u.Username, a, now)
	if err != nil {
		return nil, fmt.Errorf("rendering overview card for user %d: %w", u.ID, err)
	}
	// Stamp the card with now, the instant its analytics window is anchored to, so a
	// slower render that read older data cannot overwrite a fresher stored card (see
	// PutOverviewOGImage).
	wrote, err := st.PutOverviewOGImage(ctx, u.ID, png, now)
	if err != nil {
		return nil, fmt.Errorf("storing overview card for user %d: %w", u.ID, err)
	}
	if !wrote {
		// A concurrent render with a later window already cached a newer card, so the
		// guarded upsert skipped ours. Return that canonical card rather than the bytes
		// we just rendered: the served image must equal what the cache holds, or two
		// fetches of the same URL could unfurl different pictures. A card newer than now
		// cannot have been pruned (cleanup only drops cards older than the TTL), so this
		// reload finds it.
		canonical, err := st.OverviewOGImage(ctx, u.ID)
		if err != nil {
			return nil, fmt.Errorf("reloading canonical overview card for user %d: %w", u.ID, err)
		}
		return canonical.PNG, nil
	}
	return png, nil
}
