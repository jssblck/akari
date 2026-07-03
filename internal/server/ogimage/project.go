package ogimage

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jssblck/akari/internal/quality"
	"github.com/jssblck/akari/internal/server/store"
)

// GenerateProject renders the preview card for one project's published overview,
// stores it in the cache, and returns the encoded PNG so the caller can serve the very
// bytes it just rendered without a second read. It is the project mirror of Generate:
// the /p/<id> page renders the same aggregate panel /u/<username> does (just scoped to
// a project rather than an account), so its card is the same Render composition over
// the same windowed analytics, keyed on the project instead of the user.
//
// heading is the project title the page shows (web.ProjectTitle), passed in rather than
// derived here so this package stays free of the web view layer, the same reason
// Generate takes a resolved username. The analytics are read through AnalyticsSnapshot
// with OmitUsers set (the card shows no per-user split, matching the public project
// page), which pins the read so it cannot straddle a reparse: a reparse mid-snapshot
// reports not-ok and GenerateProject returns ErrReparseInProgress without storing
// anything, so a half-rebuilt aggregate is never cached. The window is the default
// trailing year bounded at the end of today (DefaultSince/DefaultUntil), identical to
// the window the default-range /p/<id> page computes, so the card reconciles with the
// page a visitor lands on.
//
// now fixes the analytics window and the heatmap's trailing edge; the caller passes the
// wall clock (tests pass a fixed instant).
func GenerateProject(ctx context.Context, st *store.Store, projectID int64, heading string, now time.Time) ([]byte, error) {
	// One filter scopes both reads (the same project, window, and end-of-today bound the
	// public project page uses), so the card's figures and its QUALITY grade describe the
	// same cohort the page draws.
	f := store.AnalyticsFilter{
		Since:     DefaultSince(now),
		Until:     DefaultUntil(now),
		ProjectID: projectID,
		OmitUsers: true,
	}
	// One reparse-gated snapshot reads both the token/session totals and the mean quality
	// score, so the card's figures and its QUALITY grade come from the same instant. Reading
	// the average in a second pooled query let a reparse land in the gap and cache a grade
	// that did not reconcile with the totals beside it.
	a, avg, ok, err := st.ProjectCardSnapshot(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("loading overview analytics for project %d: %w", projectID, err)
	}
	if !ok {
		return nil, ErrReparseInProgress
	}

	// The QUALITY figure is a single representative grade: the mean score across the
	// project's graded sessions in the window (the same cohort the page's Grades panel
	// draws), rounded to a letter on the standard banding. It reads "unmeasured" when no
	// session in scope is scored, so a project with no grades shows a dash rather than an F.
	qual := unmeasured
	if avg != nil {
		qual = quality.GradeFor(int(math.Round(*avg)))
	}

	png, err := Render(heading, a, now, stat{"QUALITY", qual})
	if err != nil {
		return nil, fmt.Errorf("rendering overview card for project %d: %w", projectID, err)
	}
	// Stamp the card with now, the instant its analytics window is anchored to, so a
	// slower render that read older data cannot overwrite a fresher stored card (see
	// PutProjectOGImage).
	wrote, err := st.PutProjectOGImage(ctx, projectID, png, now)
	if err != nil {
		return nil, fmt.Errorf("storing overview card for project %d: %w", projectID, err)
	}
	if !wrote {
		// A concurrent render with a later window already cached a newer card, so the
		// guarded upsert skipped ours. Return that canonical card rather than the bytes we
		// just rendered: the served image must equal what the cache holds, or two fetches
		// of the same URL could unfurl different pictures. A card newer than now cannot have
		// been pruned (cleanup only drops cards older than the TTL), so this reload finds it.
		canonical, err := st.ProjectOGImage(ctx, projectID)
		if err != nil {
			return nil, fmt.Errorf("reloading canonical overview card for project %d: %w", projectID, err)
		}
		return canonical.PNG, nil
	}
	return png, nil
}
