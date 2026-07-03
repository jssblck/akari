// Package durationfmt formats a session's wall-clock span for display. It is a neutral leaf
// (it imports only the standard library) so the two surfaces that show a session's duration,
// the web session page's Duration tile and the session Open Graph card, format it through one
// definition rather than two that could drift. The card is a faithful static preview of the
// page, so a duration that read "1m" on the card while the page showed "1m30s" would be a
// fidelity bug; sharing this formatter makes the two identical by construction.
package durationfmt

import (
	"fmt"
	"time"
)

// Span renders the interval between start and end, or "-" when it is unmeasured: either bound
// missing or zero, or end before start (a negative interval). An equal start and end is a real
// zero-length span rendered "0s", not a dash. It is the canonical session-duration string the
// web page's Duration tile shows.
func Span(start, end *time.Time) string {
	if start == nil || end == nil || start.IsZero() || end.IsZero() {
		return "-"
	}
	d := end.Sub(*start)
	if d < 0 {
		return "-"
	}
	return Positive(d)
}

// Positive renders a non-negative duration compactly: hours and minutes at or above an hour,
// minutes and seconds at or above a minute, else seconds. It is the measured branch of Span,
// exposed on its own for a caller (the OG card) that has already resolved and validated the
// span and wants the figure without Span's dash-for-unmeasured behavior, so it can substitute
// its own unmeasured marker. A zero duration renders "0s".
func Positive(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}
