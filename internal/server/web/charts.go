package web

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jssblck/akari/internal/server/store"
)

// vizPalette is the eight-step categorical data-viz ramp from DESIGN.md, ordered
// for maximum separation on the dark ground. Breakdown bars and multi-series
// charts draw from it in order, lilac first.
var vizPalette = []string{
	"#c6a8f2", // lilac
	"#88cfce", // teal
	"#f0bf92", // peach
	"#ec98b0", // rose
	"#a6d29e", // sage
	"#95c0ef", // sky
	"#ddc885", // gold
	"#a98ad4", // mauve
}

// VizColor returns the categorical color for index i, wrapping past the eighth.
func VizColor(i int) string { return vizPalette[i%len(vizPalette)] }

// chartData is the compact JSON the SVG chart module hydrates from. Days are ISO
// strings; the numeric arrays are parallel to them. encoding/json escapes <, >,
// and & by default, so this is safe to embed inside a <script> element.
type chartData struct {
	Days       []string  `json:"days"`
	Cost       []float64 `json:"cost"`
	Input      []int64   `json:"input"`
	Output     []int64   `json:"output"`
	CacheRead  []int64   `json:"cacheRead"`
	CacheWrite []int64   `json:"cacheWrite"`
}

// AnalyticsJSON marshals a session's daily series for the inline chart script.
func AnalyticsJSON(a store.Analytics) string {
	d := chartData{}
	for _, p := range a.Series {
		d.Days = append(d.Days, p.Day.UTC().Format("2006-01-02"))
		d.Cost = append(d.Cost, p.CostUSD)
		d.Input = append(d.Input, p.Input)
		d.Output = append(d.Output, p.Output)
		d.CacheRead = append(d.CacheRead, p.CacheRead)
		d.CacheWrite = append(d.CacheWrite, p.CacheWrite)
	}
	b, err := json.Marshal(d)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// BreakdownRow is one bar in a by-model or by-agent breakdown: the label, its
// formatted figures, a fill width as a percentage of the largest cost in the
// set, and its categorical color.
type BreakdownRow struct {
	Label    string
	Cost     string
	Tokens   string
	Sessions int
	Pct      float64
	Color    string
}

// BuildBreakdown turns store breakdowns into renderable bar rows. Bar width is
// proportional to cost; when every slice is unpriced (all-zero cost) it falls
// back to token share so the bars still carry information.
func BuildBreakdown(bs []store.Breakdown) []BreakdownRow {
	var maxCost float64
	var maxTok int64
	for _, b := range bs {
		if b.CostUSD > maxCost {
			maxCost = b.CostUSD
		}
		if b.Tokens > maxTok {
			maxTok = b.Tokens
		}
	}
	rows := make([]BreakdownRow, 0, len(bs))
	for i, b := range bs {
		pct := 0.0
		switch {
		case maxCost > 0:
			pct = b.CostUSD / maxCost * 100
		case maxTok > 0:
			pct = float64(b.Tokens) / float64(maxTok) * 100
		}
		// A non-zero slice always shows a sliver, so it never reads as empty.
		if pct > 0 && pct < 2 {
			pct = 2
		}
		rows = append(rows, BreakdownRow{
			Label:    b.Label,
			Cost:     FmtCost(b.CostUSD, false),
			Tokens:   FmtTokens(b.Tokens),
			Sessions: b.Sessions,
			Pct:      pct,
			Color:    VizColor(i),
		})
	}
	return rows
}

// Sparkline renders a tiny inline SVG line of daily cost for a project row in the
// index. It is purely decorative trend context, so it carries aria-hidden and no
// axis; the cost column beside it holds the real number. Returns an empty string
// when there is nothing to draw (the cell stays blank).
func Sparkline(vals []float64) string {
	if len(vals) < 2 {
		return ""
	}
	var lo, hi float64
	lo = vals[0]
	for _, v := range vals {
		if v < lo {
			lo = v
		}
		if v > hi {
			hi = v
		}
	}
	const w, h, pad = 96.0, 24.0, 2.0
	span := hi - lo
	var pts strings.Builder
	for i, v := range vals {
		x := pad + (w-2*pad)*float64(i)/float64(len(vals)-1)
		var y float64
		if span == 0 {
			y = h - pad // flat baseline when there is no variation
		} else {
			y = pad + (h-2*pad)*(1-(v-lo)/span)
		}
		if i > 0 {
			pts.WriteByte(' ')
		}
		fmt.Fprintf(&pts, "%.1f,%.1f", x, y)
	}
	line := pts.String()
	// An area path closes the polyline down to the baseline for a faint fill.
	area := fmt.Sprintf("M%.1f,%.1f L%s L%.1f,%.1f Z", pad, h-pad, line, w-pad, h-pad)
	return fmt.Sprintf(
		`<svg class="sparkline" viewBox="0 0 %g %g" preserveAspectRatio="none" aria-hidden="true">`+
			`<path d="%s" fill="rgba(198,168,242,0.14)"/>`+
			`<polyline points="%s" fill="none" stroke="#c6a8f2" stroke-width="1.25" stroke-linejoin="round" stroke-linecap="round"/>`+
			`</svg>`, w, h, area, line)
}

// The session outline (left pane of the session view) renders directly from the
// transcript messages and their tool metadata, which the page already holds in
// memory for the transcript itself. These helpers compute a row's label and tone
// during render, so the outline adds no second session-sized structure.

// OutlineTitle is a compact one-line label for an outline turn, drawn from the
// start of the message content. It scans only the first runes up to the cap
// (collapsing runs of whitespace), so its cost and allocation are bounded by the
// label length, not the message size.
func OutlineTitle(m store.Message) string {
	const max = 48
	var b strings.Builder
	b.Grow(max + 4)
	space := false   // a pending collapsed space, emitted before the next word
	started := false // have we emitted any non-space rune yet
	n := 0
	for _, r := range m.Content {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if started {
				space = true
			}
			continue
		}
		if n >= max {
			b.WriteRune('…')
			return b.String()
		}
		if space {
			b.WriteByte(' ')
			n++
			space = false
			if n >= max {
				b.WriteRune('…')
				return b.String()
			}
		}
		b.WriteRune(r)
		n++
		started = true
	}
	return b.String()
}

// OutlineStepHasBody reports whether a tool step has a stored input or result
// body, so the outline can wire only those steps to the inspector.
func OutlineStepHasBody(t store.ToolCallView) bool {
	return t.InputSHA != "" || t.ResultSHA != ""
}

// OutlineTurnClass maps a turn (its role and its tool steps) to its CSS modifier:
// role tone, with an error override so a turn with a failed tool reads in rose.
func OutlineTurnClass(role string, steps []store.ToolCallView) string {
	for _, t := range steps {
		if t.ResultStatus == "error" {
			return "ol-turn ol-error"
		}
	}
	switch role {
	case "user":
		return "ol-turn ol-user"
	case "assistant":
		return "ol-turn ol-assistant"
	default:
		return "ol-turn ol-other"
	}
}

// OutlineStepClass maps a tool step to its CSS modifier, flagging an errored step
// in rose.
func OutlineStepClass(t store.ToolCallView) string {
	if t.ResultStatus == "error" {
		return "ol-step ol-step-error"
	}
	return "ol-step"
}

// DiffTool reports whether a tool's input body is worth rendering as an inline
// diff rather than raw JSON (the file-editing tools across the three agents).
// The client reads this off the chip to decide how to expand the body.
func DiffTool(name string) bool {
	switch strings.ToLower(name) {
	case "edit", "write", "multiedit", "apply_patch", "str_replace", "str_replace_editor", "create_file", "update_file":
		return true
	}
	return false
}
