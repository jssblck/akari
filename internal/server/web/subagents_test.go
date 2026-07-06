package web

import (
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

// mkSubs builds n subagent rows, each carrying eachCost. When incompleteIdx is a
// valid index that one child is flagged unpriced, so a test can exercise the summary's
// lower-bound marker. The rows carry only what the subagents table and its summary read.
func mkSubs(n int, eachCost float64, incompleteIdx int) []store.SubagentRow {
	subs := make([]store.SubagentRow, n)
	for i := range subs {
		subs[i] = store.SubagentRow{SessionSummary: store.SessionSummary{
			ID: int64(1000 + i), Agent: "claude", Username: "grace", MessageCount: 3,
			TotalCostUSD: eachCost, CostIncomplete: i == incompleteIdx,
		}}
	}
	return subs
}

// subsView wraps rows in the SessionView shape subagentsSection renders from.
func subsView(subs []store.SubagentRow) SessionView { return SessionView{Subagents: subs} }

// TestSubagentsSummaryLabel pins the collapsed summary text: the child count with a
// singular unit at one, joined to their summed cost the same way the fan-out chip reads,
// and the "+" lower-bound marker when any child is unpriced.
func TestSubagentsSummaryLabel(t *testing.T) {
	cases := []struct {
		name string
		subs []store.SubagentRow
		want string
	}{
		{"count and summed cost", mkSubs(34, 0.18, -1), "34 subagents · $6.12"},
		{"singular at one", mkSubs(1, 0.30, -1), "1 subagent · $0.30"},
		{"an unpriced child marks the sum a lower bound",
			[]store.SubagentRow{
				{SessionSummary: store.SessionSummary{TotalCostUSD: 1.00}},
				{SessionSummary: store.SessionSummary{TotalCostUSD: 0.50}},
				{SessionSummary: store.SessionSummary{TotalCostUSD: 0.50, CostIncomplete: true}},
			},
			"3 subagents · $2.00+"},
		{"errored children add a failed clause",
			[]store.SubagentRow{
				{SessionSummary: store.SessionSummary{TotalCostUSD: 1.00}, Outcome: "errored"},
				{SessionSummary: store.SessionSummary{TotalCostUSD: 0.50}, Outcome: "completed"},
				{SessionSummary: store.SessionSummary{TotalCostUSD: 0.50}, Outcome: "errored"},
			},
			"3 subagents · $2.00 · 2 failed"},
		{"abandoned children are not failures",
			[]store.SubagentRow{
				{SessionSummary: store.SessionSummary{TotalCostUSD: 1.00}, Outcome: "abandoned"},
				{SessionSummary: store.SessionSummary{TotalCostUSD: 0.50}, Outcome: "completed"},
			},
			"2 subagents · $1.50"},
	}
	for _, c := range cases {
		if got := SubagentsSummaryLabel(c.subs); got != c.want {
			t.Errorf("%s: SubagentsSummaryLabel = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestSubagentsCollapsedThreshold pins the fold boundary: at or below the threshold the
// table reads inline, and only past it does it collapse, so the count that trips the fold
// is exactly SubagentCollapseThreshold + 1.
func TestSubagentsCollapsedThreshold(t *testing.T) {
	if SubagentsCollapsed(nil) {
		t.Error("no subagents should never collapse")
	}
	if SubagentsCollapsed(mkSubs(SubagentCollapseThreshold, 0, -1)) {
		t.Errorf("a table at the threshold (%d) should read inline, not collapse", SubagentCollapseThreshold)
	}
	if !SubagentsCollapsed(mkSubs(SubagentCollapseThreshold+1, 0, -1)) {
		t.Errorf("a table past the threshold (%d) should collapse", SubagentCollapseThreshold+1)
	}
}

// TestSubagentsBlockRender pins the three render branches: nothing when a session spawned
// no subagents, an inline table below the threshold, and a summary-headed <details> that is
// closed by default (no open attribute) above it, so a fan-out-heavy session opens folded.
func TestSubagentsBlockRender(t *testing.T) {
	if got := renderComponent(t, subagentsSection(subsView(nil), false)); strings.Contains(got, "Subagents") {
		t.Errorf("a session with no subagents should render no subagents block, got:\n%s", got)
	}

	inline := renderComponent(t, subagentsSection(subsView(mkSubs(3, 0.10, -1)), false))
	if !strings.Contains(inline, `<div class="subagents">`) {
		t.Errorf("a short subagent list should render inline, got:\n%s", inline)
	}
	if strings.Contains(inline, "<details") {
		t.Errorf("a short subagent list should not collapse, got:\n%s", inline)
	}

	folded := renderComponent(t, subagentsSection(subsView(mkSubs(12, 0.25, -1)), false))
	for _, want := range []string{
		`<details class="subagents subagents-fold">`,
		`class="subagents-summary"`,
		"12 subagents · $3.00",
	} {
		if !strings.Contains(folded, want) {
			t.Errorf("a fan-out-heavy list should collapse with a summary; missing %q, got:\n%s", want, folded)
		}
	}
	// Collapsed by default: a <details> without the open attribute starts closed, so the
	// table pays no visual cost until the reader expands it.
	if strings.Contains(folded, "<details open") || strings.Contains(folded, "open>") {
		t.Errorf("the subagents fold should start closed (no open attribute), got:\n%s", folded)
	}
}
