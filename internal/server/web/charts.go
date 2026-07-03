package web

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jssblck/akari/internal/pricing"
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
// formatted cost, the raw token volume (total plus the four-class split that
// feeds the hover card), a fill width as a percentage of the largest token volume
// in the set, and its categorical color. The token fields stay raw int64s so the
// template runs them through the same FmtTokens the rest of the app uses.
type BreakdownRow struct {
	Label      string
	Cost       string
	CostUSD    float64
	Tokens     int64
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
	Reasoning  int64
	Sessions   int
	Pct        float64
	Color      string
	// Incomplete marks a slice whose cost folded in an unpriced usage event, so the
	// row shows the same "$X+" lower-bound marker the per-session figures use.
	Incomplete bool
}

// OtherModelLabel is the bucket name for the model breakdown's long tail: every
// model without a pricing entry folds into this one row.
const OtherModelLabel = "Other"

// FoldUnknownModels collapses every model without a pricing entry into a single
// "Other" row, leaving the priced (well-known) models untouched and in their
// incoming order. Only models we have rates for surface by name on the overview;
// the rest contribute their totals to "Other" but never their IDs, so a model
// still under a codename can be exercised without leaking its name.
//
// The input is taken in its store order (cost descending); "Other" is appended
// last regardless of its totals, the usual place for a catch-all bucket. Summing
// the per-model session counts can overcount a session that spanned several
// unpriced models, the same approximation the by-model split already makes
// across its rows.
func FoldUnknownModels(bs []store.Breakdown) []store.Breakdown {
	out := make([]store.Breakdown, 0, len(bs)+1)
	other := store.Breakdown{Label: OtherModelLabel}
	var folded bool
	for _, b := range bs {
		if pricing.Known(b.Label) {
			out = append(out, b)
			continue
		}
		other.CostUSD += b.CostUSD
		other.Input += b.Input
		other.Output += b.Output
		other.CacheRead += b.CacheRead
		other.CacheWrite += b.CacheWrite
		other.Reasoning += b.Reasoning
		other.Sessions += b.Sessions
		other.CostIncomplete = other.CostIncomplete || b.CostIncomplete
		folded = true
	}
	if folded {
		out = append(out, other)
	}
	return out
}

// BuildBreakdown turns store breakdowns into renderable bar rows. Bar width is
// proportional to token volume so model and agent shares compare on the one
// figure every slice carries: a model still under a codename (unpriced, so its
// cost folds to zero) still draws a bar that reflects how much it was used.
func BuildBreakdown(bs []store.Breakdown) []BreakdownRow {
	var maxTok int64
	for _, b := range bs {
		if b.Tokens() > maxTok {
			maxTok = b.Tokens()
		}
	}
	rows := make([]BreakdownRow, 0, len(bs))
	for i, b := range bs {
		pct := 0.0
		if maxTok > 0 {
			pct = float64(b.Tokens()) / float64(maxTok) * 100
		}
		// A non-zero slice always shows a sliver, so it never reads as empty.
		if pct > 0 && pct < 2 {
			pct = 2
		}
		rows = append(rows, BreakdownRow{
			Label:      b.Label,
			Cost:       FmtCost(b.CostUSD, b.CostIncomplete),
			CostUSD:    b.CostUSD,
			Tokens:     b.Tokens(),
			Input:      b.Input,
			Output:     b.Output,
			CacheRead:  b.CacheRead,
			CacheWrite: b.CacheWrite,
			Reasoning:  b.Reasoning,
			Sessions:   b.Sessions,
			Pct:        pct,
			Color:      VizColor(i),
			Incomplete: b.CostIncomplete,
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
// start of the message content. It collapses runs of whitespace and emits at
// most 48 runes, and it never scans more than scanCap runes of input, so even a
// whitespace-heavy message costs a fixed amount, independent of message size.
func OutlineTitle(m store.Message) string {
	// An injected-context turn's content is the raw AGENTS.md / environment block, which
	// would fill the outline row with framing text; label it by kind instead so the row
	// reads as what it is and stays scannable.
	if m.Role == "context" {
		return ContextLabel(m.Content)
	}
	const max = 48      // emitted label length
	const scanCap = 256 // input runes examined, bounding the scan regardless of output
	var b strings.Builder
	b.Grow(max + 4)
	space := false   // a pending collapsed space, emitted before the next word
	started := false // have we emitted any non-space rune yet
	emitted := 0
	scanned := 0
	for _, r := range m.Content {
		if scanned >= scanCap {
			if started {
				b.WriteRune('…')
			}
			return b.String()
		}
		scanned++
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if started {
				space = true
			}
			continue
		}
		if emitted >= max {
			b.WriteRune('…')
			return b.String()
		}
		if space {
			b.WriteByte(' ')
			emitted++
			space = false
			if emitted >= max {
				b.WriteRune('…')
				return b.String()
			}
		}
		b.WriteRune(r)
		emitted++
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
	case "context":
		return "ol-turn ol-context"
	default:
		return "ol-turn ol-other"
	}
}

// ContextLabel names what an injected-context turn carries, from the marker its
// content opens with (see parser.isCodexContext). It is a short, fixed label for the
// outline row and the transcript disclosure summary, never the raw framing text.
func ContextLabel(content string) string {
	t := strings.TrimSpace(content)
	hasAgents := strings.HasPrefix(t, "# AGENTS.md instructions for ") || strings.HasPrefix(t, "<user_instructions>")
	hasEnv := strings.Contains(t, "<environment_context>")
	switch {
	case hasAgents && hasEnv:
		return "project instructions + environment"
	case hasAgents:
		return "project instructions"
	case hasEnv:
		return "environment"
	default:
		return "agent context"
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
