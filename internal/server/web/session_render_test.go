package web

import (
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// floatPtr returns a pointer to a float, for the nullable per-turn cost in TurnUsage fixtures.
func floatPtr(f float64) *float64 { return &f }

// viewFor assembles the SessionView a render test hands SessionPage: the fixture's
// messages serve as both the outline read and the (whole-session) transcript window,
// the tool map feeds both the whole-session surfaces (outline, ribbon) and the window's
// rows, and the header's fallbacks double as the window's, the shape a short real
// session loads with (the snapshot reads fill both from one transaction), so assertions
// against the rendered page cover both surfaces.
func viewFor(d store.SessionDetail, msgs []store.Message, tools map[int][]store.ToolCallView, subs []store.SubagentRow, hs HeaderStats, dupIDs int) SessionView {
	return SessionView{
		Detail:      d,
		Outline:     msgs,
		Page:        store.TranscriptPage{Msgs: msgs, Fallbacks: hs.Fallbacks},
		Tools:       tools,
		WindowTools: tools,
		Subagents:   subs,
		Header:      hs,
		DupIDs:      dupIDs,
	}
}

// fixtureDetail is a codex shell_command's Detail: a realistic invocation longer
// than DetailLabel's 80-rune cap, so the fixture exercises truncation rather than
// a string that happens to fit.
const fixtureDetail = "./scripts/stop-slop.sh --check --format=jsonl --input transcript.jsonl --output findings.jsonl"

// sessionFixture is a representative session detail for render assertions: a
// remote project session with a couple of turns and a tool call whose bodies live
// in the CAS, so the chip renders its stamps as body-opening buttons.
func sessionFixture() (store.SessionDetail, []store.Message, map[int][]store.ToolCallView) {
	start := time.Date(2026, 6, 28, 9, 2, 0, 0, time.UTC)
	end := start.Add(90 * time.Minute)
	d := store.SessionDetail{
		SessionSummary: store.SessionSummary{
			ID:               1826,
			Agent:            "codex",
			Machine:          "grace",
			GitBranch:        "main",
			Username:         "jessoteric",
			MessageCount:     5,
			UserMessageCount: 3,
			TotalInput:       180237,
			TotalOutput:      12248,
			TotalCacheRead:   807552,
			TotalCacheWrite:  4096,
			TotalCostUSD:     1.67,
			Visibility:       "internal",
			StartedAt:        &start,
			EndedAt:          &end,
		},
		ProjectID:   7,
		ProjectKey:  "jssblck/dots",
		ProjectName: "dots",
		ProjectKind: "remote",
	}
	msgs := []store.Message{
		{Ordinal: 0, Role: "user", Content: "Run the guarded rerun.", Timestamp: &start},
		{Ordinal: 1, Role: "assistant", Content: "Running the stop-slop pass.", Model: "gpt-5", HasToolUse: true, Timestamp: &start},
	}
	tools := map[int][]store.ToolCallView{
		1: {{
			MessageOrdinal: 1, ToolName: "shell_command", FilePath: "scripts/stop-slop.sh", Detail: fixtureDetail,
			InputSHA: "aaaa", InputBytes: 143, InputMediaType: "json",
			ResultSHA: "bbbb", ResultBytes: 5800, ResultMediaType: "text", ResultStatus: "ok",
		}},
	}
	return d, msgs, tools
}

// TestSessionPageRendersSubagents pins that a parent session renders its linked subagents as
// a nested list. The link that fills this (parent_session_id) is set at ingest; before it
// was written the list was always empty, so this guards the now-live path. With no subagents
// the heading is absent rather than an empty table.
func TestSessionPageRendersSubagents(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	subs := []store.SubagentRow{
		{SessionSummary: store.SessionSummary{ID: 4242, Agent: "claude", Username: "grace", Machine: "grace", GitBranch: "main", MessageCount: 11, TotalCostUSD: 0.42}},
		{SessionSummary: store.SessionSummary{ID: 4243, Agent: "claude", Username: "grace", Machine: "grace", GitBranch: "main", MessageCount: 7, TotalCostUSD: 0.19}},
	}

	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, subs, HeaderStats{}, 0), false, true))
	for _, want := range []string{
		`<h2>Subagents</h2>`,
		`class="subagents"`,
		`/sessions/4242`, // each child links to its own session
		`/sessions/4243`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("session page with subagents missing %q", want)
		}
	}

	bare := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, HeaderStats{}, 0), false, true))
	if strings.Contains(bare, "<h2>Subagents</h2>") {
		t.Error("session page without subagents should omit the Subagents heading")
	}
}

// A tool call's Detail (the bounded, human-scannable summary of its input) renders
// on both surfaces that show a tool call: the transcript chip and the outline step
// nested under its turn. Outline renders from the same tools map SessionPage passes
// it, so this asserts against SessionPage's output rather than calling Outline
// directly. templ HTML-escapes attribute values, so the title assertion checks the
// escaped form; the fixture detail has no characters that need escaping, so the
// raw and escaped forms coincide here.
func TestToolCallDetailRendersOnChipAndOutline(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, HeaderStats{}, 0), false, true))

	label := DetailLabel(fixtureDetail)
	if label == fixtureDetail {
		t.Fatalf("fixture detail must exceed the 80-rune cap to exercise truncation, got label %q", label)
	}

	for _, want := range []string{
		`class="tdetail muted" title="` + fixtureDetail + `"` + `>` + label + `<`, // the chip
		`data-detail="` + fixtureDetail + `"`,                                     // the inspect-open outline step carries the full text for app.js
		`class="ol-detail" title="` + fixtureDetail + `"` + `>` + label + `<`,     // the outline step's own label
	} {
		if !strings.Contains(html, want) {
			t.Errorf("session page missing %q", want)
		}
	}

	// A call with no Detail (an old CAS-stripped session, or a tool like Read whose
	// chip already shows FilePath) renders neither span, so the page degrades to
	// today's output exactly.
	bareTools := map[int][]store.ToolCallView{
		1: {{MessageOrdinal: 1, ToolName: "Read", FilePath: "internal/server/web/session.templ"}},
	}
	bare := renderComponent(t, SessionPage(p, viewFor(d, msgs, bareTools, nil, HeaderStats{}, 0), false, true))
	for _, unwant := range []string{`class="tdetail`, `class="ol-detail`, `data-detail`} {
		if strings.Contains(bare, unwant) {
			t.Errorf("a tool call with no Detail should render no %q", unwant)
		}
	}
}

// TestTranscriptRendersContextTurn pins that an injected-context message renders as a
// collapsed disclosure (its own Context section) rather than an open user turn: the
// framing is available but folded away, the outline labels it by kind instead of
// dumping the raw AGENTS.md text, and the real prompt after it is the first open turn.
func TestTranscriptRendersContextTurn(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, _, _ := sessionFixture()
	base := time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC)
	ts := func(secs int) *time.Time { u := base.Add(time.Duration(secs) * time.Second); return &u }

	agents := "# AGENTS.md instructions for /home/ada/akari\n\nRun make build.\n<environment_context>\n  <cwd>/home/ada/akari</cwd>\n</environment_context>"
	msgs := []store.Message{
		{Ordinal: 0, Role: "context", Content: agents, Timestamp: ts(0)},
		{Ordinal: 1, Role: "user", Content: "Add rate limiting", Timestamp: ts(1)},
		{Ordinal: 2, Role: "assistant", Content: "On it.", Model: "gpt-5", Timestamp: ts(6)},
	}
	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, nil, nil, HeaderStats{}, 0), false, true))

	for _, want := range []string{
		// The context turn renders as a collapsed <details>, not an open .msg-user block.
		`class="msg msg-context context-turn"`,
		`id="msg-0"`,
		// Its summary names the kind rather than showing the raw framing text.
		`class="tag context-kind">project instructions + environment</span>`,
		// The outline row for the context turn is labeled by kind and carries the context tone.
		`class="ol-turn ol-context"`,
		`project instructions + environment`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("context transcript missing %q", want)
		}
	}
	// The framing must not be rendered as a user turn: no msg-user block carries the AGENTS.md text.
	if strings.Contains(html, `class="msg msg-user"`) && strings.Contains(html, `>msg-0<`) {
		t.Error("the context turn must not render as a user message")
	}
}

// TestContextLabelAndRoleClass pins the context helpers' branches directly: ContextLabel
// names the framing by which markers its content carries (both, project-only,
// environment-only, or an unrecognized fallback), and RoleClass maps the context role to
// its own CSS class so a context message styled through the shared .msg path still reads
// as context rather than falling through to the generic tone.
func TestContextLabelAndRoleClass(t *testing.T) {
	labelCases := []struct {
		name    string
		content string
		want    string
	}{
		{"both markers", "# AGENTS.md instructions for /x\n<environment_context>\n</environment_context>", "project instructions + environment"},
		{"user_instructions plus env", "<user_instructions>\ndo x\n</user_instructions>\n<environment_context></environment_context>", "project instructions + environment"},
		{"project only", "# AGENTS.md instructions for /x\n\nRun make build.", "project instructions"},
		{"environment only", "<environment_context>\n  <cwd>/x</cwd>\n</environment_context>", "environment"},
		{"fallback", "some other injected framing", "agent context"},
	}
	for _, c := range labelCases {
		t.Run("label/"+c.name, func(t *testing.T) {
			if got := ContextLabel(c.content); got != c.want {
				t.Errorf("ContextLabel(%q) = %q, want %q", c.content, got, c.want)
			}
		})
	}

	if got := RoleClass("context"); got != "msg-context" {
		t.Errorf("RoleClass(\"context\") = %q, want msg-context", got)
	}
}

// The redesigned session header carries its controls inline and opens tool bodies
// in a modal: an owner of an internal session gets a compact actions cluster with
// Publish and Delete, the full-width page wrapper, and the modal overlay host. The
// old stacked publish banners and the density toggle must be gone.
func TestSessionPageCompactHeaderAndModal(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, HeaderStats{}, 0), false, true))

	for _, want := range []string{
		`class="session-page"`,    // full-width wrapper drives main:has(> .session-page)
		`class="session-actions"`, // inline control cluster
		`>Publish</button>`,       // internal owner can publish
		`>Delete</button>`,        // owner can delete
		`id="session-modal"`,      // overlay host
		`id="session-inspector"`,  // dialog the body renders into
		`role="dialog"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("session page missing %q", want)
		}
	}
	for _, unwant := range []string{
		`class="publish card"`,   // old publish banner
		`class="publish subtle"`, // old delete banner
		`>Delete session</button>`,
		`data-density`, // density toggle removed
		`>Comfortable<`, `>Compact<`,
		`class="inspector"`, // docked inspector column removed
	} {
		if strings.Contains(html, unwant) {
			t.Errorf("session page should no longer render %q", unwant)
		}
	}
}

// The session stat strip folds input, output, cache read, cache write, and cost
// into one Tokens tile carrying its hover breakdown, and the always-on "live"
// badge is gone from the header.
func TestSessionStatsFoldTokensAndDropLive(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, HeaderStats{}, 0), true, true))

	// The tile must show the aggregate of all four token classes, and the tooltip
	// must carry each class value plus the cost, run through the same formatters
	// the loose tiles used. A nonzero cache-write guards that field specifically.
	total := FmtTokens(d.TotalInput + d.TotalOutput + d.TotalCacheRead + d.TotalCacheWrite)
	for _, want := range []string{
		`class="stat tokens-stat"`,                   // the folded tile
		`data-stat-key="tokens">` + total + `</div>`, // the aggregate, flashed on live change
		`class="stat-tip"`,                           // its hover breakdown
		`>Cache read</dt>`, `>Cache write</dt>`,      // the per-class labels
		`<dd>` + FmtTokens(d.TotalInput) + `</dd>`,                                // input value
		`<dd>` + FmtTokens(d.TotalCacheWrite) + `</dd>`,                           // cache-write value (nonzero)
		`class="tt-cost">` + FmtCost(d.TotalCostUSD, d.CostIncomplete) + `</div>`, // cost text
	} {
		if !strings.Contains(html, want) {
			t.Errorf("session stats missing %q", want)
		}
	}
	for _, unwant := range []string{
		`data-stat-key="input"`, `data-stat-key="cw"`, `data-stat-key="cost"`, // the old loose tiles
		`class="live-dot"`, // the always-on live badge
	} {
		if strings.Contains(html, unwant) {
			t.Errorf("session stats should no longer render %q", unwant)
		}
	}
}

// The Tokens tile adds a Context sub-group when the session's context was measured: the
// heaviest single-turn context it held and how many times it shed that context. A session
// with no measured context (nil signals) draws no Context group, so an unmeasured session
// never shows a misleading zero.
func TestSessionStatsTokensContext(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	peak := int64(142000)
	resets := 2
	hs := HeaderStats{Signals: store.SessionSignals{
		Outcome: "completed", OutcomeConfidence: "high",
		PeakContextTokens: &peak, ContextResetCount: &resets,
	}}
	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, hs, 0), false, true))

	for _, want := range []string{
		`class="tt-sub">Context</div>`,           // the context group under its own ruled label
		`>Peak context</dt>`, `<dd>142,000</dd>`, // the peak, full thousands-separated tokens
		`>Resets</dt>`, `<dd>2</dd>`, // the inferred reset count
		`It measures context load, not spend.`, // the caption scoping the peak to a single class, not a tokens total
	} {
		if !strings.Contains(html, want) {
			t.Errorf("tokens context group missing %q", want)
		}
	}

	// A session with no measured context draws no Context group at all.
	bare := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, HeaderStats{}, 0), false, true))
	if strings.Contains(bare, `class="tt-sub">Context</div>`) {
		t.Error("an unmeasured session should draw no Context group in the Tokens tile")
	}
}

// The Quality tile shows the letter grade as its headline, banded by the report-card
// class, and reveals the outcome, confidence, score, and tool-health counts in its
// tooltip. A scored session reads its grade; the tool-health rows appear only when the
// session ran tools. The prompt-hygiene signals that fired read under a separate Input
// group, and a clean signal (zero duplicates here) draws no row.
func TestSessionStatsQualityTileScored(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	score := 82
	grade := "B"
	hs := HeaderStats{Signals: store.SessionSignals{
		Outcome: "completed", OutcomeConfidence: "high",
		Score: &score, Grade: &grade,
		ToolCalls: 12, ToolFailures: 2, ToolRetries: 1, EditChurn: 3, LongestFailureStreak: 1,
		ShortPromptCount: 4, NoCodeContextCount: 2, UnstructuredStart: true,
	}}
	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, hs, 0), false, true))

	for _, want := range []string{
		`class="stat quality-stat q-good"`,    // a B bands as "good"
		`data-stat-key="quality">B</div>`,     // the grade headline, flashed on live change
		`>Outcome</dt>`, `<dd>Completed</dd>`, // outcome row, title-cased
		`>Confidence</dt>`, `<dd>high</dd>`, // confidence row
		`>Score</dt>`, `<dd>82 / 100</dd>`, // numeric score
		`>Failures</dt>`, `<dd>2 / 12</dd>`, // tool-health rows (the session ran tools)
		`>Longest fail streak</dt>`, `<dd>1</dd>`,
		`class="tt-sub">Input</div>`,        // the hygiene group under its own ruled label
		`>Terse prompts</dt>`, `<dd>4</dd>`, // a hygiene row that fired
		`>No code pointer</dt>`, `<dd>2</dd>`,
		`>Opening</dt>`, `<dd>terse</dd>`, // the unstructured-start flag
	} {
		if !strings.Contains(html, want) {
			t.Errorf("quality tile missing %q", want)
		}
	}
	if strings.Contains(html, `>Repeated</dt>`) {
		t.Error("a clean hygiene signal (zero duplicates) should draw no Repeated row")
	}
}

// An unscored session (an unknown outcome with no tool signal) shows a neutral dash
// rather than a letter, bands as "none", and says "not scored" with no tool-health rows.
func TestSessionStatsQualityTileUnscored(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	hs := HeaderStats{Signals: store.SessionSignals{Outcome: "unknown", OutcomeConfidence: "low"}}
	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, hs, 0), false, true))

	for _, want := range []string{
		`class="stat quality-stat q-none"`,
		`data-stat-key="quality">-</div>`, // a dash, never a grade
		`<dd>Unknown</dd>`,
		`<dd>not scored</dd>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("unscored quality tile missing %q", want)
		}
	}
	if strings.Contains(html, `>Failures</dt>`) {
		t.Error("an unscored, tool-less session should show no tool-health rows")
	}
}

// The transcript reads as an instrumented trace: an answered turn carries a latency stamp, a
// user message renders its hygiene tags, a context-shed divider draws above the shed-down turn,
// a message with usage carries its context and cost stamps, and a tool chip prefers the
// worktree-relative path with the absolute one in its title. This drives the per-message
// instruments end to end through the transcript template.
func TestTranscriptInstruments(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, _, _ := sessionFixture()
	base := time.Date(2026, 6, 28, 9, 0, 0, 0, time.UTC)
	ts := func(secs int) *time.Time { u := base.Add(time.Duration(secs) * time.Second); return &u }

	// A prompt (terse and code-pointerless), its reply 6s later, then a heavy turn and a
	// shed-down turn so a context divider draws between ordinals 2 and 3. Each turn's usage now
	// rides on its message (Message.Usage), the shape the message read folds it into.
	msgs := []store.Message{
		{Ordinal: 0, Role: "user", Content: "fix it", Timestamp: ts(0),
			PromptShort: true, PromptNoCode: true, PromptDigest: 4242, PromptFactsCurrent: true},
		{Ordinal: 1, Role: "assistant", Content: "On it.", Model: "gpt-5", Timestamp: ts(6),
			Usage: &store.TurnUsage{Input: 1200, Output: 950, CacheRead: 78000, CacheWrite: 3100, ContextTokens: 82300, CostUSD: floatPtr(0.31), CostIncomplete: true}},
		{Ordinal: 2, Role: "user", Content: "and the tests too please, thorough pass", Timestamp: ts(20),
			PromptDigest: 7, PromptFactsCurrent: true,
			Usage: &store.TurnUsage{Input: 1000, CacheRead: 159000, CacheWrite: 0, ContextTokens: 160000}},
		{Ordinal: 3, Role: "assistant", Content: "Compacted.", Model: "gpt-5", Timestamp: ts(31),
			Usage: &store.TurnUsage{Input: 500, CacheRead: 11500, CacheWrite: 0, ContextTokens: 12000}},
	}
	tools := map[int][]store.ToolCallView{
		1: {{
			MessageOrdinal: 1, ToolName: "Edit",
			FilePath: "C:\\Users\\me\\projects\\worktrees\\akari\\x\\internal\\auth.go", FileRelPath: "internal/auth.go",
			InputSHA: "aaaa", InputBytes: 143, InputMediaType: "json",
		}},
	}

	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, HeaderStats{}, 0), false, true))

	for _, want := range []string{
		// (a) the answered turn's latency stamp, prompt-to-reply
		`class="stamp-latency mono" title="time from the prompt to this reply">+6s</span>`,
		// (b) the hygiene tags on the terse, no-code prompt
		`class="tag hygiene" title="under 4 words: give the agent something to grip">terse</span>`,
		`class="tag hygiene" title="a change request with no file, path, or code anchor">no code pointer</span>`,
		// (c) the shed divider between the heavy turn and the shed-down turn, hosting its
		// before/after breakdown card
		`class="msg-shed tok-cell"`,
		`context shed: 160.0k → 12.0k`,
		`class="tok-tip shed-tip"`,    // the shed divider's breakdown card
		`>Before</dt>`, `>After</dt>`, // the two turns' occupancy, each with its per-class split
		// (d) the context and cost stamps ride one focusable host that carries a single per-turn
		// breakdown card (the four classes, the context total, the cost)
		`class="tok-cell turn-metrics"`,
		`class="stamp-ctx mono">ctx 82.3k</span>`,
		// A mixed priced/unpriced turn's cost reads as a lower bound ("$0.31+") both on the stamp
		// and in the card, so an exact-looking cost never sits beside unpriced token rows.
		`class="stamp-cost mono">$0.31+</span>`,
		`class="tok-tip"`,               // the shared breakdown card markup
		`>Context</dt>`,                 // the context-occupancy line inside the turn card
		`<dd>82,300</dd>`,               // ordinal 1's context total, full tokens
		`<dd>78,000</dd>`,               // its cache read, a per-class row in the card
		`class="tt-cost">$0.31+</span>`, // the turn cost in the card, marked a lower bound
		// (e) the tool chip prefers the relative path, absolute in the title
		`title="C:\Users\me\projects\worktrees\akari\x\internal\auth.go">internal/auth.go</span>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("instrumented transcript missing %q", want)
		}
	}
	// The unpriced turn (ordinal 2, nil cost) still shows its ctx stamp, and its breakdown card
	// reads "unpriced" for the cost rather than a misleading $0.00.
	if !strings.Contains(html, `ctx 160.0k`) {
		t.Error("a turn with usage but no priced cost should still show its context stamp")
	}
	if !strings.Contains(html, `class="tt-cost">unpriced</span>`) {
		t.Error("an unpriced turn's breakdown card should read \"unpriced\" for its cost")
	}
}

// The quality tooltip appends a score-arithmetic group when the session is scored: one row per
// penalty (label plus the negative points), then the final score. A clean scored run (no
// penalties) reads "no penalties" instead of an empty group; an unscored session draws no group.
func TestSessionQualityScoreArithmetic(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	score := 61
	grade := "D"
	hs := HeaderStats{Signals: store.SessionSignals{
		Outcome: "errored", OutcomeConfidence: "high",
		Score: &score, Grade: &grade,
		ToolCalls: 10, ToolFailures: 3, // 30 errored + 9 failures = 39, score 61
	}}
	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, hs, 0), false, true))
	for _, want := range []string{
		`class="tt-sub">Score arithmetic</div>`,
		`>errored ending</dt>`, `<dd class="mono">-30</dd>`,
		`>3 tool failures</dt>`, `<dd class="mono">-9</dd>`,
		`>Score</dt>`, `<dd class="mono">61</dd>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("score arithmetic missing %q", want)
		}
	}

	cs, cg := 100, "A"
	clean := HeaderStats{Signals: store.SessionSignals{
		Outcome: "completed", OutcomeConfidence: "high", Score: &cs, Grade: &cg,
	}}
	cleanHTML := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, clean, 0), false, true))
	if !strings.Contains(cleanHTML, `>No penalties</dt>`) {
		t.Error("a clean scored run should show a No penalties row in the score arithmetic")
	}

	unscored := HeaderStats{Signals: store.SessionSignals{Outcome: "unknown", OutcomeConfidence: "low"}}
	unscoredHTML := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, unscored, 0), false, true))
	if strings.Contains(unscoredHTML, `Score arithmetic`) {
		t.Error("an unscored session should draw no score-arithmetic group")
	}
}

// A published session owned by the viewer swaps Publish for Unpublish and surfaces
// the public share path as a link.
func TestSessionPagePublicShowsUnpublish(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	pub := "k3y"
	d.Visibility = "public"
	d.PublicID = &pub
	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, HeaderStats{}, 0), false, true))

	for _, want := range []string{`>Unpublish</button>`, `class="share-link`, PublicPath(pub)} {
		if !strings.Contains(html, want) {
			t.Errorf("public session page missing %q", want)
		}
	}
	if strings.Contains(html, `>Publish</button>`) {
		t.Error("a public session should not offer Publish")
	}
}

// A non-owner admin can delete but not publish; a plain logged-in viewer gets no
// actions cluster at all.
func TestSessionPageActionsGating(t *testing.T) {
	d, msgs, tools := sessionFixture()

	admin := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "ada", IsAdmin: true}
	adminHTML := renderComponent(t, SessionPage(admin, viewFor(d, msgs, tools, nil, HeaderStats{}, 0), false, false))
	if !strings.Contains(adminHTML, `>Delete</button>`) {
		t.Error("admin should be able to delete a session they do not own")
	}
	if strings.Contains(adminHTML, `>Publish</button>`) {
		t.Error("a non-owner admin should not see Publish")
	}

	viewer := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "anna"}
	viewerHTML := renderComponent(t, SessionPage(viewer, viewFor(d, msgs, tools, nil, HeaderStats{}, 0), false, false))
	if strings.Contains(viewerHTML, `class="session-actions"`) {
		t.Error("a plain viewer should see no actions cluster")
	}
}

// The public (logged-out) session page takes the same full-width wrapper and modal
// host, and drops the density toggle, so a published session reads with the same
// ergonomics as the owner's view.
func TestPublicSessionPageWrapperAndModal(t *testing.T) {
	d, msgs, tools := sessionFixture()
	pub := "k3y"
	d.Visibility = "public"
	d.PublicID = &pub
	v := viewFor(d, msgs, tools, nil, HeaderStats{}, 0)
	html := renderComponent(t, PublicSessionPage(v, OGMeta{}))

	for _, want := range []string{`class="session-page"`, `id="session-modal"`, `id="session-inspector"`} {
		if !strings.Contains(html, want) {
			t.Errorf("public session page missing %q", want)
		}
	}
	if strings.Contains(html, `data-density`) {
		t.Error("public session page should not render the density toggle")
	}
}
