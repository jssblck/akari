package web

import (
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

// TestInsightsSummary pins the summary strip's sentences and its guards: the GPA is
// session-weighted over the graded cohort on the A=4..F=0 scale, spend leads with the total
// and pulls the abandoned share out, tools report a failure rate over a thousands-separated
// call count, and every sentence drops out when its figure is unmeasured rather than printing
// a zero the page cannot stand behind.
func TestInsightsSummary(t *testing.T) {
	full := store.Insights{
		Quality: store.QualityDistribution{
			// A=2, B=1 over 3 graded: 2*4 + 1*3 = 11 points, GPA 11/3 = 3.666... -> 3.67.
			Grades:   []store.LabeledCount{{Key: "A", Count: 2}, {Key: "B", Count: 1}, {Key: "", Count: 1}},
			Sessions: 4,
			Graded:   3,
		},
		Tools: store.ToolStats{TotalCalls: 1500, TotalFailures: 30}, // 2.0% over 1,500 calls
		Trends: &store.Trends{
			Economics: store.Economics{TotalSpend: 100, TotalAbandoned: 12, AbandonedSharePct: 12},
		},
	}
	got := InsightsSummary(full)
	want := []string{
		"Graded 3 of 4 sessions at GPA 3.67.",
		"Spend totaled $100.00, with 12% ($12.00) sunk into abandoned sessions.",
		"Tools failed on 2.0% of 1,500 calls.",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d sentences, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("sentence[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// No graded session drops the quality line rather than claiming a GPA of zero.
	noGrade := full
	noGrade.Quality = store.QualityDistribution{Sessions: 4, Graded: 0}
	if lines := InsightsSummary(noGrade); strings.Contains(strings.Join(lines, " "), "GPA") {
		t.Errorf("ungraded scope should emit no quality sentence, got %#v", lines)
	}

	// Spend with nothing abandoned states the total alone, no waste clause.
	clean := full
	clean.Trends = &store.Trends{Economics: store.Economics{TotalSpend: 50}}
	lines := InsightsSummary(clean)
	if !containsLine(lines, "Spend totaled $50.00.") {
		t.Errorf("clean spend should read as the total alone, got %#v", lines)
	}

	// A sub-percent abandoned share (a few cents out of hundreds) rounds to 0% and must not
	// print "with 0% (...) sunk", which reads as broken. The spend line stands alone.
	trace := full
	trace.Trends = &store.Trends{Economics: store.Economics{TotalSpend: 182.49, TotalAbandoned: 0.12, AbandonedSharePct: 0.066}}
	traceLines := InsightsSummary(trace)
	if !containsLine(traceLines, "Spend totaled $182.49.") {
		t.Errorf("a sub-percent abandoned share should drop the waste clause, got %#v", traceLines)
	}
	if strings.Contains(strings.Join(traceLines, " "), "sunk") {
		t.Errorf("a sub-percent abandoned share must drop the waste clause entirely, got %#v", traceLines)
	}

	// A nil trend grid and a zero-spend window both drop the spend line.
	for _, ins := range []store.Insights{
		{Quality: full.Quality, Tools: full.Tools, Trends: nil},
		{Quality: full.Quality, Tools: full.Tools, Trends: &store.Trends{Economics: store.Economics{TotalSpend: 0}}},
	} {
		if strings.Contains(strings.Join(InsightsSummary(ins), " "), "Spend totaled") {
			t.Errorf("unmeasured spend should emit no spend sentence, got %#v", InsightsSummary(ins))
		}
	}

	// No tool calls drops the reliability line.
	noTools := full
	noTools.Tools = store.ToolStats{}
	if strings.Contains(strings.Join(InsightsSummary(noTools), " "), "Tools failed") {
		t.Errorf("a window with no tool calls should emit no tools sentence")
	}

	// A scope with nothing measurable yields an empty strip.
	if lines := InsightsSummary(store.Insights{}); len(lines) != 0 {
		t.Errorf("an empty scope should yield no sentences, got %#v", lines)
	}
}

func containsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}
