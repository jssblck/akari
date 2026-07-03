package web

import (
	"testing"
	"time"

	"github.com/jssblck/akari/internal/durationfmt"
)

// TestFmtDurationDelegatesToDurationfmt pins that the session page's Duration tile formats
// through the shared durationfmt helper. The session OG card formats the same figure through
// the same helper, so this guards the property the OG-card review depends on: the card's
// DURATION cannot drift from the page's. A future edit that reintroduced a bespoke format here
// would break this and be caught before it shipped a mismatched card.
func TestFmtDurationDelegatesToDurationfmt(t *testing.T) {
	base := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	ptr := func(tm time.Time) *time.Time { return &tm }
	spans := []struct{ start, end *time.Time }{
		{ptr(base), ptr(base.Add(90 * time.Second))},
		{ptr(base), ptr(base.Add(100 * time.Minute))},
		{ptr(base), ptr(base)},                 // zero-length: both render "0s"
		{ptr(base), ptr(base.Add(-time.Hour))}, // negative: both dash
		{nil, ptr(base)},                       // unmeasured: both dash
	}
	for _, s := range spans {
		if got, want := FmtDuration(s.start, s.end), durationfmt.Span(s.start, s.end); got != want {
			t.Errorf("FmtDuration(%v, %v) = %q, want %q (must match the shared formatter the card uses)", s.start, s.end, got, want)
		}
	}
}
