// Package web holds akari's server-rendered UI: templ templates and the small
// view-model helpers they use. Handlers in the httpapi package resolve auth,
// query the store, and render these templates, so all rendering lives here.
package web

import (
	"fmt"

	"github.com/jssblck/akari/internal/server/store"
)

// FmtBytes renders a byte count compactly (the tool-body metadata chips).
func FmtBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// FmtCost renders a USD cost. Sub-cent costs still show enough precision to be
// meaningful.
func FmtCost(usd float64, incomplete bool) string {
	var s string
	switch {
	case usd == 0:
		s = "$0"
	case usd < 0.01:
		s = fmt.Sprintf("$%.4f", usd)
	default:
		s = fmt.Sprintf("$%.2f", usd)
	}
	if incomplete {
		s += "+"
	}
	return s
}

// FmtPercent renders a 0..1 fraction as a whole-number percent, for the cache hit
// rate. A real but tiny rate (under 1%) rounds up to 1% rather than 0%, so a scope
// that did hit the cache never reads as a total miss; a true zero stays 0%.
func FmtPercent(f float64) string {
	if f <= 0 {
		return "0%"
	}
	p := f * 100
	if p < 1 {
		return "1%"
	}
	return fmt.Sprintf("%.0f%%", p)
}

// FmtSavings renders a cache saving for the Cache tile. A non-negative saving reads as
// "saved $X"; the rare negative, where cache was written but never re-read enough to
// repay the creation premium, reads as "cost $X" on its magnitude, so the figure stays
// honest without printing a minus sign into a "saved" label.
//
// An incomplete saving reads "... partial", NOT the "$X+" lower-bound marker the cost
// figures use. A saving omitted for an unpriced model can be negative (a Claude cache
// write is priced above input), so the true figure could be lower OR higher than shown:
// "partial" says it is incomplete without implying a direction the data cannot support.
func FmtSavings(usd float64, incomplete bool) string {
	verb := "saved "
	if usd < 0 {
		verb, usd = "cost ", -usd
	}
	s := verb + FmtCost(usd, false)
	if incomplete {
		s += " partial"
	}
	return s
}

// FmtTokens renders a token count with thousands separators.
func FmtTokens(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	s := fmt.Sprintf("%d", n)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// FmtTokensCompact renders a token count to a short magnitude (2.1B, 1.7M, 63.0k,
// 412), for the feed's inline figure where the exact value lives in the hover
// card. The thousands-separated FmtTokens stays the form for places that show the
// full number. Keep these buckets aligned with the React formatter so a figure
// reads the same on either surface.
func FmtTokensCompact(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.1fB", float64(n)/1_000_000_000)
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// RowTokens is a session's total token volume across all four classes (input,
// output, cache read, cache write), matching the overview heatmap's notion of a
// day's "total tokens" so the figure and its breakdown agree across views.
func RowTokens(s store.SessionSummary) int64 {
	return s.TotalInput + s.TotalOutput + s.TotalCacheRead + s.TotalCacheWrite
}
