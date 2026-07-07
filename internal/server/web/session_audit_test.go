package web

import (
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

func strPtr(s string) *string { return &s }

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
