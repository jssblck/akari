package web

import (
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

func TestFmtCompletionRate(t *testing.T) {
	cases := map[float64]string{
		-1:               "-",
		0:                "0%",
		66.6666666666667: "67%",
		100:              "100%",
	}
	for in, want := range cases {
		if got := FmtCompletionRate(in); got != want {
			t.Errorf("FmtCompletionRate(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestFmtGPA(t *testing.T) {
	cases := map[float64]string{
		-1:  "-",
		0:   "0.0",
		2.4: "2.4",
		4:   "4.0",
	}
	for in, want := range cases {
		if got := FmtGPA(in); got != want {
			t.Errorf("FmtGPA(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestAttentionReasonLabel(t *testing.T) {
	cases := map[string]string{
		"errored":   "errored",
		"abandoned": "abandoned",
		"grade-f":   "graded F",
		"grade-d":   "graded D",
		"costly":    "costly run",
	}
	for key, want := range cases {
		if got := AttentionReasonLabel(store.AttentionRow{Reason: key}); got != want {
			t.Errorf("AttentionReasonLabel(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestAttentionReasonTone(t *testing.T) {
	cases := map[string]string{
		"errored":   "err",
		"abandoned": "err",
		"grade-f":   "warn",
		"grade-d":   "warn",
		"costly":    "warn",
	}
	for key, want := range cases {
		if got := AttentionReasonTone(store.AttentionRow{Reason: key}); got != want {
			t.Errorf("AttentionReasonTone(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestCompletionTone(t *testing.T) {
	cases := []struct {
		completed, settled int
		want               string
	}{
		{0, 0, ""},    // nothing settled: unmeasured, no verdict colour
		{9, 10, "ok"}, // 90%
		{8, 10, "ok"}, // 80% is the ok floor
		{6, 10, "warn"},
		{5, 10, "warn"}, // 50% is the warn floor
		{4, 10, "err"},
	}
	for _, c := range cases {
		au := store.AuditSummary{Completed: c.completed, Settled: c.settled}
		if got := CompletionTone(au); got != c.want {
			t.Errorf("CompletionTone(%d/%d) = %q, want %q", c.completed, c.settled, got, c.want)
		}
	}
}

func TestGPATone(t *testing.T) {
	cases := []struct {
		points float64
		graded int
		want   string
	}{
		{0, 0, ""},       // no graded session: unmeasured
		{35, 10, "ok"},   // 3.5
		{30, 10, "ok"},   // 3.0 is the ok floor
		{25, 10, "warn"}, // 2.5
		{20, 10, "warn"}, // 2.0 is the warn floor
		{10, 10, "err"},  // 1.0
	}
	for _, c := range cases {
		au := store.AuditSummary{GradePoints: c.points, Graded: c.graded}
		if got := GPATone(au); got != c.want {
			t.Errorf("GPATone(%v over %d) = %q, want %q", c.points, c.graded, got, c.want)
		}
	}
}

// TestAuditDashRenders pins the Overview audit lead: with work in scope it draws the four
// verdict tiles and the flagged sessions, each linking to its transcript, and it draws
// nothing at all on an empty scope so the usage panel's own empty state shows through.
func TestAuditDashRenders(t *testing.T) {
	p := Page{Title: "Overview", LoggedIn: true, Active: "overview", Username: "Grace Hopper"}
	grade := "F"
	au := store.AuditSummary{
		WorkItems: 5, Settled: 4, Completed: 3, Wasted: 1,
		Graded: 3, GradePoints: 9, WastedUSD: 2.5,
		Attention: []store.AttentionRow{{
			ID: 42, Agent: "claude", ProjectName: "akari", ProjectKind: "remote",
			Grade: &grade, Outcome: "errored", CostUSD: 5.0, Reason: "errored",
			Title: "rebuild the insights page",
		}},
	}
	html := renderComponent(t, OverviewPage(p, analyticsWithData(), au, DefaultRange, nil, nil))

	for _, want := range []string{
		"Needs attention",
		">Work<", ">Completed<", ">Quality<", ">Spend<",
		"errored",             // the reason chip
		`href="/sessions/42"`, // the flagged row links to its transcript
		"rebuild the insights page",
		"1 flagged",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("populated audit dash missing %q", want)
		}
	}

	// An empty scope (no work items) draws no verdict at all, so the usage panel's empty
	// state is what the reader sees rather than a strip of dashes.
	empty := renderComponent(t, OverviewPage(p, analyticsWithData(), store.AuditSummary{}, DefaultRange, nil, nil))
	if strings.Contains(empty, "Needs attention") {
		t.Error("empty audit scope should not render the attention shortlist")
	}
}
