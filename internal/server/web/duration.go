// Package web holds akari's server-rendered UI: templ templates and the small
// view-model helpers they use. Handlers in the httpapi package resolve auth,
// query the store, and render these templates, so all rendering lives here.
package web

import (
	"context"
	"fmt"
	"time"

	"github.com/jssblck/akari/internal/durationfmt"
)

// FmtTime renders a timestamp in the viewer's timezone (UTC when none is set), or a
// dash when absent. It keeps the bare "2006-01-02 15:04" form for a visible cell;
// FmtTimeLong adds the zone abbreviation for a hover title, where naming the zone
// earns its width.
func FmtTime(ctx context.Context, t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.In(Loc(ctx)).Format("2006-01-02 15:04")
}

// FmtTimeLong is FmtTime with the zone abbreviation appended, for the hover title
// on a stamp shown short elsewhere. Naming the zone (PST, UTC, ...) lets a reader
// tell which zone a full stamp is in without cluttering every visible cell with it.
func FmtTimeLong(ctx context.Context, t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.In(Loc(ctx)).Format("2006-01-02 15:04 MST")
}

// FmtRelTime renders a timestamp as a coarse "time ago" for the recent past
// (today, 1 day ago, ...), falling back to an absolute stamp once it is a week
// or more old, where a relative phrasing stops being useful. It reads "now" from
// the wall clock; relTime holds the testable core. It backs the "Updated" column
// on both the projects index and the per-project session table, so the two read
// alike (the global feed groups by day instead and uses FeedTime).
func FmtRelTime(ctx context.Context, t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return relTime(time.Now(), *t, Loc(ctx))
}

// relTime is FmtRelTime's clock-injected core. Day distance is measured between
// calendar dates in the viewer's zone (not 24-hour windows), so a session from
// late last night reads "1 day ago" rather than "today" merely because fewer than
// 24 hours have passed, and the boundary is the reader's local midnight rather than
// UTC's. The absolute-stamp fallback also renders in the viewer's zone.
func relTime(now, t time.Time, loc *time.Location) string {
	now, t = now.In(loc), t.In(loc)
	nd := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	td := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	days := int(nd.Sub(td).Hours() / 24)
	switch {
	case days <= 0: // today, or a future stamp from clock skew
		return "today"
	case days == 1:
		return "1 day ago"
	case days < 7:
		return fmt.Sprintf("%d days ago", days)
	default:
		return t.Format("2006-01-02 15:04")
	}
}

// FmtTimeAt renders a non-pointer timestamp in the viewer's timezone, or a dash
// when zero.
func FmtTimeAt(ctx context.Context, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.In(Loc(ctx)).Format("2006-01-02 15:04")
}

// FmtDuration renders the span between start and end, or a dash. It delegates to durationfmt
// so the session page's Duration tile and the session OG card, which both show this figure,
// format it through one definition and cannot drift apart.
func FmtDuration(start, end *time.Time) string {
	return durationfmt.Span(start, end)
}
