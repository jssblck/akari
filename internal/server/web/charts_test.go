package web

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

func TestBuildBreakdownCostWidths(t *testing.T) {
	rows := BuildBreakdown([]store.Breakdown{
		{Label: "claude-opus-4-8", CostUSD: 4, Tokens: 100, Sessions: 2},
		{Label: "gpt-5.5", CostUSD: 1, Tokens: 50, Sessions: 1},
	})
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].Pct != 100 {
		t.Errorf("largest cost should be full width, got %.2f", rows[0].Pct)
	}
	if rows[1].Pct != 25 {
		t.Errorf("second bar should be 25%% of max, got %.2f", rows[1].Pct)
	}
	if rows[0].Color != vizPalette[0] || rows[1].Color != vizPalette[1] {
		t.Errorf("bars take categorical colors in order: %s, %s", rows[0].Color, rows[1].Color)
	}
	if rows[0].Cost != "$4.00" {
		t.Errorf("cost formatting: got %q", rows[0].Cost)
	}
}

func TestBuildBreakdownTokenFallback(t *testing.T) {
	// All-zero cost falls back to token share for the bar width.
	rows := BuildBreakdown([]store.Breakdown{
		{Label: "a", CostUSD: 0, Tokens: 80},
		{Label: "b", CostUSD: 0, Tokens: 40},
	})
	if rows[0].Pct != 100 || rows[1].Pct != 50 {
		t.Errorf("token fallback widths wrong: %.2f, %.2f", rows[0].Pct, rows[1].Pct)
	}
}

func TestBuildBreakdownTinySliver(t *testing.T) {
	rows := BuildBreakdown([]store.Breakdown{
		{Label: "big", CostUSD: 100},
		{Label: "tiny", CostUSD: 0.5},
	})
	if rows[1].Pct < 2 {
		t.Errorf("a non-zero slice should keep a visible sliver, got %.2f", rows[1].Pct)
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

func TestBuildRail(t *testing.T) {
	msgs := []store.Message{{Ordinal: 0, Role: "user"}, {Ordinal: 1, Role: "assistant"}}
	tools := map[int][]store.ToolCallView{
		1: {{ResultStatus: "ok"}, {ResultStatus: "error"}},
	}
	rail := BuildRail(msgs, tools)
	if len(rail) != 2 {
		t.Fatalf("want 2 markers, got %d", len(rail))
	}
	if rail[0].ToolCount != 0 || rail[0].HasError {
		t.Error("user turn has no tools and no error")
	}
	if rail[1].ToolCount != 2 || !rail[1].HasError {
		t.Errorf("assistant turn should count 2 tools and flag the error: %+v", rail[1])
	}
	if RailClass(rail[1]) != "rail-tick rail-error" {
		t.Errorf("an errored turn reads in rose regardless of role: %s", RailClass(rail[1]))
	}
	if RailClass(rail[0]) != "rail-tick rail-user" {
		t.Errorf("user tick class: %s", RailClass(rail[0]))
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
