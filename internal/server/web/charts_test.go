package web

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

func TestBuildBreakdownTokenWidths(t *testing.T) {
	// Bar width tracks token volume, not cost: the cheaper-per-token model can
	// still draw the longer bar when it ran more tokens.
	rows := BuildBreakdown([]store.Breakdown{
		{Label: "claude-opus-4-8", CostUSD: 4, Input: 100, Sessions: 2},
		{Label: "gpt-5.5", CostUSD: 1, Input: 25, Sessions: 1},
	})
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].Pct != 100 {
		t.Errorf("largest token count should be full width, got %.2f", rows[0].Pct)
	}
	if rows[1].Pct != 25 {
		t.Errorf("second bar should be 25%% of max tokens, got %.2f", rows[1].Pct)
	}
	if rows[0].Color != vizPalette[0] || rows[1].Color != vizPalette[1] {
		t.Errorf("bars take categorical colors in order: %s, %s", rows[0].Color, rows[1].Color)
	}
	if rows[0].Cost != "$4.00" {
		t.Errorf("cost formatting: got %q", rows[0].Cost)
	}
}

func TestBuildBreakdownTokensIgnoreCost(t *testing.T) {
	// A high-cost slice with few tokens stays short; width is token-driven only.
	rows := BuildBreakdown([]store.Breakdown{
		{Label: "pricey", CostUSD: 100, Input: 10},
		{Label: "chatty", CostUSD: 1, Input: 100},
	})
	if rows[0].Pct != 10 {
		t.Errorf("cost should not widen the bar, got %.2f", rows[0].Pct)
	}
	if rows[1].Pct != 100 {
		t.Errorf("the token leader should be full width, got %.2f", rows[1].Pct)
	}
}

func TestBuildBreakdownCountsCacheTokens(t *testing.T) {
	// The bar sizes on the all-class volume, not just uncached in/out. This is the
	// fix for the dashboard reading as mispriced: a Claude slice whose volume is
	// almost all cache must out-width a slice with more in/out but no cache, and the
	// row must carry the four-class split so the hover card can break it back out.
	rows := BuildBreakdown([]store.Breakdown{
		{Label: "claude-opus-4-8", CostUSD: 942, Input: 1_000_000, Output: 500_000, CacheRead: 80_000_000, CacheWrite: 12_000_000},
		{Label: "gpt-5.5", CostUSD: 57, Input: 4_000_000, Output: 2_500_000},
	})
	claude, gpt := rows[0], rows[1]
	if claude.Tokens <= gpt.Tokens {
		t.Fatalf("cache-heavy slice should carry the larger volume: claude=%d gpt=%d", claude.Tokens, gpt.Tokens)
	}
	if claude.Tokens != 93_500_000 {
		t.Errorf("total should sum all four classes, got %d", claude.Tokens)
	}
	if claude.Pct != 100 || gpt.Pct >= 100 {
		t.Errorf("the cache-heavy slice should be full width and lead: claude=%.2f gpt=%.2f", claude.Pct, gpt.Pct)
	}
	// Every class is pinned to a distinct value so wiring a field to the wrong
	// source (In showing the output total, say) fails rather than passing by luck.
	if claude.Input != 1_000_000 || claude.Output != 500_000 {
		t.Errorf("row must keep the in/out split for the tooltip: in=%d out=%d", claude.Input, claude.Output)
	}
	if claude.CacheRead != 80_000_000 || claude.CacheWrite != 12_000_000 {
		t.Errorf("row must keep the cache split for the tooltip: read=%d write=%d", claude.CacheRead, claude.CacheWrite)
	}
}

// The breakdown bar renders its token figure as the all-class total carrying the
// same hover card every other token figure has: hovering the number must surface
// the four-class split and the cost, not leave a bare in+out figure beside an
// unexplained cost. This is the rendered half of the cache-token fix.
func TestBreakdownListTokenTooltip(t *testing.T) {
	rows := BuildBreakdown([]store.Breakdown{
		{Label: "claude-opus-4-8", CostUSD: 942.08, Input: 1_000_000, Output: 500_000, CacheRead: 80_000_000, CacheWrite: 12_000_000, Sessions: 67},
	})
	html := renderComponent(t, breakdownList("By model", rows))

	for _, want := range []string{
		`class="tok-cell"`, // the hover container wraps the figure
		`class="tok-total">` + FmtTokens(93_500_000) + `<`, // headline is the all-class total
		`class="tok-tip"`, // the breakdown card
		`<dt>In</dt><dd>` + FmtTokens(1_000_000) + `</dd>`,
		`<dt>Out</dt><dd>` + FmtTokens(500_000) + `</dd>`,
		`<dt>Cache read</dt><dd>` + FmtTokens(80_000_000) + `</dd>`,
		`<dt>Cache write</dt><dd>` + FmtTokens(12_000_000) + `</dd>`,
		`class="tt-cost">$942.08</span>`, // the cost the volume actually bought
		`67 sessions`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("breakdown bar missing %q", want)
		}
	}
	// The old bare "input+output tok" figure (1,500,000) must not be what's shown.
	if strings.Contains(html, `class="tok-total">`+FmtTokens(1_500_000)+`<`) {
		t.Error("breakdown still showing the uncached in+out figure as the headline")
	}
}

func TestBuildBreakdownZeroTokens(t *testing.T) {
	// Width is token-driven with no cost fallback: when no slice has tokens, every
	// bar stays at 0% even though cost is present.
	rows := BuildBreakdown([]store.Breakdown{
		{Label: "a", CostUSD: 5},
		{Label: "b", CostUSD: 1},
	})
	if rows[0].Pct != 0 || rows[1].Pct != 0 {
		t.Errorf("zero-token bars should stay at 0%%, got %.2f, %.2f", rows[0].Pct, rows[1].Pct)
	}
}

func TestBuildBreakdownTinySliver(t *testing.T) {
	rows := BuildBreakdown([]store.Breakdown{
		{Label: "big", Input: 10000},
		{Label: "tiny", Input: 1},
	})
	if rows[1].Pct < 2 {
		t.Errorf("a non-zero slice should keep a visible sliver, got %.2f", rows[1].Pct)
	}
}

func TestFoldUnknownModels(t *testing.T) {
	// Priced models keep their identity and order; everything unpriced (including
	// a codenamed pre-release) collapses into a single trailing "Other" row.
	// The unpriced models carry a distinct value in every class so the fold is
	// checked to sum all four, not just Input: dropping Output, CacheRead, or
	// CacheWrite from the Other row would fail below.
	folded := FoldUnknownModels([]store.Breakdown{
		{Label: "claude-opus-4-8", CostUSD: 4, Input: 100, Sessions: 2},
		{Label: "skunkworks-preview", CostUSD: 0, Input: 30, Output: 11, CacheRead: 700, CacheWrite: 9, Sessions: 1},
		{Label: "gpt-5.5", CostUSD: 1, Input: 50, Sessions: 1},
		{Label: "internal-codename-x", CostUSD: 0, Input: 20, Output: 4, CacheRead: 300, CacheWrite: 1, Sessions: 2},
	})
	if len(folded) != 3 {
		t.Fatalf("want 3 rows (two priced + Other), got %d: %+v", len(folded), folded)
	}
	if folded[0].Label != "claude-opus-4-8" || folded[1].Label != "gpt-5.5" {
		t.Errorf("priced models should keep order and name: %+v", folded[:2])
	}
	other := folded[2]
	if other.Label != OtherModelLabel {
		t.Fatalf("the catch-all row should sit last and be %q, got %q", OtherModelLabel, other.Label)
	}
	for _, row := range folded {
		if strings.Contains(row.Label, "preview") || strings.Contains(row.Label, "codename") {
			t.Errorf("an unpriced model name leaked into the overview: %q", row.Label)
		}
	}
	// Each class folds independently: In 30+20, Out 11+4, cache read 700+300,
	// cache write 9+1, and the total is their sum (1075).
	if other.Input != 50 || other.Output != 15 || other.CacheRead != 1000 || other.CacheWrite != 10 {
		t.Errorf("Other should fold every class of the unpriced tail: in=%d out=%d read=%d write=%d",
			other.Input, other.Output, other.CacheRead, other.CacheWrite)
	}
	if other.Tokens() != 1075 || other.Sessions != 3 {
		t.Errorf("Other should sum the unpriced tail: tokens=%d sessions=%d", other.Tokens(), other.Sessions)
	}
}

func TestFoldUnknownModelsAllKnown(t *testing.T) {
	// With every model priced, nothing folds and no "Other" row appears.
	folded := FoldUnknownModels([]store.Breakdown{
		{Label: "claude-opus-4-8", CostUSD: 4},
		{Label: "gpt-5.5", CostUSD: 1},
	})
	if len(folded) != 2 {
		t.Fatalf("no Other row when all models are priced, got %d rows", len(folded))
	}
	for _, row := range folded {
		if row.Label == OtherModelLabel {
			t.Errorf("unexpected Other row: %+v", folded)
		}
	}
}

func TestSparklineEmptyAndShape(t *testing.T) {
	if Sparkline(nil) != "" || Sparkline([]float64{1}) != "" {
		t.Error("a sparkline needs at least two points")
	}
	svg := Sparkline([]float64{0, 2, 1, 3})
	if !strings.Contains(svg, "<svg") || !strings.Contains(svg, "<polyline") {
		t.Errorf("sparkline should be an svg polyline: %s", svg)
	}
	// A flat series must not divide by zero; it renders a baseline.
	if flat := Sparkline([]float64{5, 5, 5}); !strings.Contains(flat, "polyline") {
		t.Errorf("flat sparkline should still render: %s", flat)
	}
}

func TestOutlineTitle(t *testing.T) {
	// Leading/internal/trailing whitespace collapses to single spaces, one line.
	if got := OutlineTitle(store.Message{Content: "  Reorganize   the\nUI please "}); got != "Reorganize the UI please" {
		t.Errorf("title = %q", got)
	}
	// Empty / whitespace-only content yields an empty title (the view shows the role).
	if got := OutlineTitle(store.Message{Content: "   \n\t "}); got != "" {
		t.Errorf("blank title = %q", got)
	}
	// Content beyond the cap is truncated with an ellipsis and never longer than
	// the cap (+ the ellipsis rune), regardless of the message size.
	long := ""
	for i := 0; i < 500; i++ {
		long += "word "
	}
	got := OutlineTitle(store.Message{Content: long})
	if []rune(got)[len([]rune(got))-1] != '…' {
		t.Errorf("a long title should end with an ellipsis: %q", got)
	}
	if n := len([]rune(got)); n > 50 {
		t.Errorf("a long title should stay bounded, got %d runes: %q", n, got)
	}
}

func TestOutlineClasses(t *testing.T) {
	steps := []store.ToolCallView{
		{ToolName: "Edit", ResultStatus: "ok", InputSHA: "deadbeef"},
		{ToolName: "Bash", ResultStatus: "error"},
	}
	// A turn with a failed step reads in rose regardless of role.
	if got := OutlineTurnClass("assistant", steps); got != "ol-turn ol-error" {
		t.Errorf("errored turn class = %q", got)
	}
	if got := OutlineTurnClass("assistant", steps[:1]); got != "ol-turn ol-assistant" {
		t.Errorf("assistant turn class = %q", got)
	}
	if got := OutlineTurnClass("user", nil); got != "ol-turn ol-user" {
		t.Errorf("user turn class = %q", got)
	}
	if got := OutlineTurnClass("system", nil); got != "ol-turn ol-other" {
		t.Errorf("default turn class = %q", got)
	}
	if got := OutlineStepClass(steps[1]); got != "ol-step ol-step-error" {
		t.Errorf("errored step class = %q", got)
	}
	if got := OutlineStepClass(steps[0]); got != "ol-step" {
		t.Errorf("ok step class = %q", got)
	}
	if !OutlineStepHasBody(steps[0]) || OutlineStepHasBody(steps[1]) {
		t.Errorf("step body detection wrong: %+v", steps)
	}
}

func TestBaseName(t *testing.T) {
	cases := map[string]string{
		"a/b/c.txt":     "c.txt",
		"c.txt":         "c.txt",
		"a\\b\\c.txt":   "c.txt",
		"/leading/x.go": "x.go",
		"":              "",
	}
	for in, want := range cases {
		if got := BaseName(in); got != want {
			t.Errorf("BaseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDiffTool(t *testing.T) {
	for _, name := range []string{"Edit", "edit", "Write", "MultiEdit", "apply_patch", "str_replace"} {
		if !DiffTool(name) {
			t.Errorf("%q should render as a diff", name)
		}
	}
	for _, name := range []string{"Bash", "Read", "Grep", ""} {
		if DiffTool(name) {
			t.Errorf("%q should not render as a diff", name)
		}
	}
}

func TestAnalyticsJSONRoundTrip(t *testing.T) {
	day := time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC)
	a := store.Analytics{Series: []store.DayPoint{
		{Day: day, CostUSD: 1.25, Input: 100, Output: 20},
		{Day: day.AddDate(0, 0, 1), CostUSD: 0.5, Input: 60, Output: 10},
	}}
	var got chartData
	if err := json.Unmarshal([]byte(AnalyticsJSON(a)), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Days) != 2 || got.Days[0] != "2026-06-03" {
		t.Errorf("days wrong: %v", got.Days)
	}
	if got.Cost[0] != 1.25 || got.Input[1] != 60 {
		t.Errorf("series values wrong: %+v", got)
	}
}
