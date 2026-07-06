package web

import (
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

func strPtr(s string) *string { return &s }
func scored(outcome string, score int, grade string) store.SessionSignals {
	return store.SessionSignals{Outcome: outcome, OutcomeConfidence: "high", Score: intPtr(score), Grade: strPtr(grade)}
}

// TestAuditHeaderLeadsWithVerdict pins workstream D-1: the session view opens with the
// audit answer. A scored errored run leads with its grade and outcome in the error
// tone, its score, a wasted tile carrying the run's cost, and the score arithmetic
// promoted to a visible line.
func TestAuditHeaderLeadsWithVerdict(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	hs := HeaderStats{Signals: scored("errored", 55, "F")}
	hs.Signals.ToolCalls = 20
	hs.Signals.ToolFailures = 5

	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, hs, 0), false, true))
	for _, want := range []string{
		`id="session-instruments"`,
		`class="verdict session-verdict"`,
		`F · errored`,
		`vvalue tone-err`,
		`scored 55 / 100`,
		`class="score-arith mono muted small"`,
		`= 55`,
		`>Wasted</div>`,
		`this run errored`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("audit header missing %q", want)
		}
	}
}

// TestAuditHeaderPendingGrade pins the C3 fix: an unscored session names that grading
// happens at settle instead of leaving a bare dash that reads as missing data, and
// renders no wasted tile and no arithmetic line.
func TestAuditHeaderPendingGrade(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()

	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, HeaderStats{}, 0), false, true))
	if !strings.Contains(html, "graded after the session settles") {
		t.Error("an unscored session should name the settle deferral (C3)")
	}
	if strings.Contains(html, ">Wasted<") {
		t.Error("a session with no waste should render no Wasted tile")
	}
	if strings.Contains(html, "score-arith") {
		t.Error("an unscored session has no arithmetic to show")
	}
}

// TestAuditHeaderWorkItemRollup pins the B-2 rollup on the detail page: a fan-out
// session's Cost tile names the whole work item's cost and size, and the model line
// rides the Duration tile.
func TestAuditHeaderWorkItemRollup(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	v := viewFor(d, msgs, tools, nil, HeaderStats{Signals: scored("completed", 100, "A")}, 0)
	v.Tree = store.TreeRollup{SubagentCount: 34, CostUSD: 6.12}
	v.Models = []string{"claude-fable-5", "claude-haiku-4-5"}

	html := renderComponent(t, SessionPage(p, v, false, true))
	for _, want := range []string{
		"work item $6.12 across 34 subagents",
		"claude-fable-5, claude-haiku-4-5",
		"A · completed",
		"100, no penalties",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("audit header missing %q", want)
		}
	}
}

// TestSessionWasted pins the wasted-spend fold: the run's own cost counts when it
// errored or was abandoned, errored children always count, abandoned children never
// do, and a healthy run wastes nothing.
func TestSessionWasted(t *testing.T) {
	d := store.SessionDetail{SessionSummary: store.SessionSummary{TotalCostUSD: 2.00}}
	subs := []store.SubagentRow{
		{SessionSummary: store.SessionSummary{TotalCostUSD: 0.50}, Outcome: "errored"},
		{SessionSummary: store.SessionSummary{TotalCostUSD: 0.25}, Outcome: "abandoned"},
		{SessionSummary: store.SessionSummary{TotalCostUSD: 1.00}, Outcome: "completed"},
	}

	usd, _, note := SessionWasted(d, store.SessionSignals{Outcome: "errored"}, subs)
	if usd != 2.50 {
		t.Fatalf("errored run + errored child = $%.2f, want $2.50", usd)
	}
	if !strings.Contains(note, "this run errored") || !strings.Contains(note, "1 failed subagent") {
		t.Fatalf("note = %q", note)
	}

	usd, _, note = SessionWasted(d, store.SessionSignals{Outcome: "completed"}, subs)
	if usd != 0.50 || note != "1 failed subagent" {
		t.Fatalf("completed run keeps only child waste, got $%.2f %q", usd, note)
	}

	usd, _, note = SessionWasted(d, store.SessionSignals{Outcome: "completed"}, nil)
	if usd != 0 || note != "" {
		t.Fatalf("a healthy run wastes nothing, got $%.2f %q", usd, note)
	}
}

// TestScoreArithmeticLine pins the promoted breakdown line: 100 minus each penalty
// equals the score, "no penalties" for a clean run, absent while unscored.
func TestScoreArithmeticLine(t *testing.T) {
	s := scored("errored", 55, "F")
	s.ToolCalls = 20
	s.ToolFailures = 5
	line := ScoreArithmeticLine(s)
	if !strings.HasPrefix(line, "100 - ") || !strings.HasSuffix(line, "= 55") {
		t.Fatalf("arithmetic line = %q", line)
	}
	if got := ScoreArithmeticLine(scored("completed", 100, "A")); got != "100, no penalties" {
		t.Fatalf("clean run line = %q", got)
	}
	if got := ScoreArithmeticLine(store.SessionSignals{}); got != "" {
		t.Fatalf("unscored line = %q, want empty", got)
	}
}

// TestFlowRibbonTicks pins D-4: one tick per turn past the visibility gate, colored by
// activity with failure outranking edit outranking run, and no ribbon at all for a
// short session whose shape is visible by scrolling.
func TestFlowRibbonTicks(t *testing.T) {
	msgs := make([]store.Message, 14)
	for i := range msgs {
		role := "assistant"
		if i%2 == 0 {
			role = "user"
		}
		msgs[i] = store.Message{Ordinal: i, Role: role, Content: "m"}
	}
	tools := map[int][]store.ToolCallView{
		1: {{ToolName: "Edit", Category: "edit"}},
		3: {{ToolName: "Bash", Category: "bash"}},
		5: {{ToolName: "Edit", Category: "edit"}, {ToolName: "Bash", Category: "bash", ResultStatus: "error"}},
	}

	html := renderComponent(t, flowRibbon(SessionView{Outline: msgs, Tools: tools}, false))
	for _, want := range []string{
		`id="session-flow"`,
		`class="flow"`,
		`flow-tick ft-edit" href="#msg-1"`,
		`flow-tick ft-run" href="#msg-3"`,
		`flow-tick ft-fail" href="#msg-5"`, // failure outranks the edit on the same turn
		`flow-tick ft-user" href="#msg-0"`,
		`flow-tick ft-plain" href="#msg-7"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("flow ribbon missing %q", want)
		}
	}

	// A short session renders the stable wrapper (the live append's out-of-band
	// target) but no ticks.
	short := renderComponent(t, flowRibbon(SessionView{Outline: msgs[:6], Tools: tools}, false))
	if strings.Contains(short, "flow-tick") {
		t.Error("a short session should render no ribbon ticks")
	}
	if !strings.Contains(short, `id="session-flow"`) {
		t.Error("the ribbon wrapper must always render so the append swap has a target")
	}
}

// TestTranscriptWindowEarlierBar pins the windowed transcript's top edge: a window with
// earlier turns renders the bar with its keyset cursor and remaining count, and a
// whole-session window renders none.
func TestTranscriptWindowEarlierBar(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	v := viewFor(d, msgs, tools, nil, HeaderStats{}, 0)
	v.Page.HasEarlier = true
	v.Page.EarlierCount = 213

	html := renderComponent(t, SessionPage(p, v, false, true))
	for _, want := range []string{
		`id="transcript-earlier"`,
		`hx-get="/sessions/1826/body?before=0"`,
		`hx-swap="outerHTML"`,
		"213 earlier messages",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("earlier bar missing %q", want)
		}
	}

	whole := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, HeaderStats{}, 0), false, true))
	if strings.Contains(whole, "transcript-earlier") {
		t.Error("a whole-session window should render no earlier bar")
	}
}

// TestTranscriptAppendFragment pins the incremental SSE fragment: the new rows arrive
// bare (they append into the existing .transcript), and the instruments and subagents
// ride along as out-of-band swaps so the verdict and tiles stay current without a
// full-body re-render.
func TestTranscriptAppendFragment(t *testing.T) {
	d, msgs, tools := sessionFixture()
	v := viewFor(d, msgs, tools, nil, HeaderStats{}, 0)

	html := renderComponent(t, TranscriptAppend(v))
	for _, want := range []string{
		`id="msg-0"`,
		`id="session-instruments" hx-swap-oob="outerHTML"`,
		`id="session-subagents" hx-swap-oob="outerHTML"`,
		`id="session-flow" hx-swap-oob="outerHTML"`,
		`id="session-outline" class="outline" aria-label="Session outline" hx-swap-oob="outerHTML"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("append fragment missing %q", want)
		}
	}
	if strings.Contains(html, `<div class="transcript">`) {
		t.Error("an append fragment must not open its own transcript container")
	}

	// A quiet tick (no rows) keeps the instrument refreshes but not the
	// whole-session shape.
	quiet := v
	quiet.Page = store.TranscriptPage{}
	qhtml := renderComponent(t, TranscriptAppend(quiet))
	if !strings.Contains(qhtml, `id="session-instruments"`) {
		t.Error("a quiet append should still refresh the instruments")
	}
	if strings.Contains(qhtml, `id="session-flow"`) || strings.Contains(qhtml, `id="session-outline"`) {
		t.Error("a quiet append should not ship the ribbon or outline")
	}
}

// TestSubagentsTableVerdicts pins the outcome column: an errored child reads its
// outcome word in the error tone with its grade chip, a completed child stays muted,
// and an unmeasured child shows nothing rather than a dash.
func TestSubagentsTableVerdicts(t *testing.T) {
	subs := []store.SubagentRow{
		{SessionSummary: store.SessionSummary{ID: 1, Agent: "claude", Title: "Verify the payment flow"}, Outcome: "errored", Grade: strPtr("F")},
		{SessionSummary: store.SessionSummary{ID: 2, Agent: "claude"}, Outcome: "completed", Grade: strPtr("A")},
		{SessionSummary: store.SessionSummary{ID: 3, Agent: "claude"}},
	}
	html := renderComponent(t, subagentsSection(subsView(subs), false))
	for _, want := range []string{
		`class="sub-outcome tone-err"`,
		`>errored</span>`,
		`class="sub-outcome muted"`,
		"Verify the payment flow",
		`claude subagent`, // the no-title fallback
		`tag grade q-poor`,
		`tag grade q-good`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("subagents table missing %q", want)
		}
	}
}
