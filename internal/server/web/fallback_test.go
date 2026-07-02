package web

import (
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

func intPtr(i int) *int       { return &i }
func int64Ptr(i int64) *int64 { return &i }

// The badge label reads "fallback" for one and "fallback xN" (the multiplication sign,
// not the letter) for more, and its title states the fact in a full sentence. The feed
// row carries no served model, so the title names only that it fell back from Fable 5.
func TestFallbackBadgeLabelAndTitle(t *testing.T) {
	if got := FallbackBadgeLabel(1); got != "fallback" {
		t.Errorf("one fallback = %q, want %q", got, "fallback")
	}
	if got := FallbackBadgeLabel(3); got != "fallback ×3" {
		t.Errorf("three fallbacks = %q, want %q", got, "fallback ×3")
	}
	// The multiplication sign, not the ASCII letter x.
	if strings.Contains(FallbackBadgeLabel(2), "x") {
		t.Error("the badge must use the multiplication sign, not the letter x")
	}
	if got := FallbackBadgeTitle(1); got != "1 turn fell back from Fable 5 to a lower model" {
		t.Errorf("one-title = %q", got)
	}
	if got := FallbackBadgeTitle(2); !strings.HasPrefix(got, "2 turns fell back") {
		t.Errorf("plural title = %q", got)
	}
}

// A feed row shows the warn-toned fallback tag only when the session had a fallback, with
// the count for two or more and the fuller sentence in its title. A row with no fallback
// draws no tag, so the badge's absence reads as "no fallback".
func TestGlobalSessionListFallbackBadge(t *testing.T) {
	ts := time.Now().UTC().Add(-2 * 24 * time.Hour)
	rows := []store.SessionRow{{
		SessionSummary: store.SessionSummary{
			ID: 11, Agent: "claude", GitBranch: "main", Username: "ada",
			MessageCount: 4, ModelFallbackCount: 2, LastActiveAt: &ts,
		},
		ProjectID: 3, ProjectKey: "akari", ProjectName: "akari", ProjectKind: "remote",
	}}
	html := renderComponent(t, GlobalSessionList(rows, store.SessionFilter{Sort: "updated", Desc: true}, SessionFooter{Shown: 1}))
	for _, want := range []string{
		`class="tag warn"`,
		`fallback ×2</span>`,
		`title="2 turns fell back from Fable 5 to a lower model"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("feed row missing fallback badge %q", want)
		}
	}

	// A session with no fallback draws no badge.
	rows[0].ModelFallbackCount = 0
	bare := renderComponent(t, GlobalSessionList(rows, store.SessionFilter{Sort: "updated", Desc: true}, SessionFooter{Shown: 1}))
	if strings.Contains(bare, ">fallback") {
		t.Error("a session with no fallback should draw no fallback badge")
	}
}

// The header renders the Fallbacks tile only when the session had a fallback, and its
// tooltip lists each one: the model pair (a real arrow glyph), the refusal category, the
// declined attempt's token spend, and the time. A session with no fallback draws no tile,
// so its absence is the negative signal.
func TestSessionStatsFallbackTile(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	d.ModelFallbackCount = 1
	when := time.Date(2026, 6, 28, 9, 5, 0, 0, time.UTC)
	hs := HeaderStats{Fallbacks: []store.ModelFallback{{
		MessageOrdinal:  intPtr(1),
		FromModel:       "claude-fable-5",
		ToModel:         "claude-opus-4-8",
		Trigger:         "reasoning_extraction",
		RefusalCategory: "reasoning_extraction",
		DeclinedInput:   int64Ptr(1200),
		DeclinedOutput:  int64Ptr(300),
		OccurredAt:      &when,
	}}}
	html := renderComponent(t, SessionPage(p, d, msgs, tools, nil, nil, hs, 0, false, true))

	for _, want := range []string{
		`class="stat fallback-stat"`,
		`data-stat-key="fallbacks">1</div>`,
		`>Models</dt>`, `<dd>claude-fable-5 → claude-opus-4-8</dd>`, // the model pair, arrow glyph
		`>Category</dt>`, `<dd>reasoning_extraction</dd>`,
		`>Declined tokens</dt>`, `<dd>1,500</dd>`, // input + output, summed and separated
		`>Time</dt>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("fallback tile missing %q", want)
		}
	}

	// A session with no fallback draws no tile at all: absence is the signal.
	d.ModelFallbackCount = 0
	bare := renderComponent(t, SessionPage(p, d, msgs, tools, nil, nil, HeaderStats{}, 0, false, true))
	if strings.Contains(bare, `class="stat fallback-stat"`) {
		t.Error("a session with no fallback should draw no Fallbacks tile")
	}
}

// A fallback whose fields never merged in (empty models, empty category, nil counts)
// reads plain defaults in the tile rather than blank cells, and omits the token row.
func TestFallbackTileDefensiveDefaults(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	d.ModelFallbackCount = 1
	hs := HeaderStats{Fallbacks: []store.ModelFallback{{DedupKey: "x"}}}
	html := renderComponent(t, SessionPage(p, d, msgs, tools, nil, nil, hs, 0, false, true))

	if !strings.Contains(html, `<dd>unknown → unknown</dd>`) {
		t.Error("an unnamed model pair should read unknown, not blank")
	}
	if !strings.Contains(html, `<dd>uncategorized</dd>`) {
		t.Error("an empty refusal category should read uncategorized")
	}
	if !strings.Contains(html, `<dd>-</dd>`) {
		t.Error("a fallback with no timestamp should read a dash")
	}
	if strings.Contains(html, `>Declined tokens</dt>`) {
		t.Error("a fallback with no measured spend should omit the declined-tokens row")
	}
}

// The transcript marks the turn a fallback landed on with a slim warn-styled notice
// naming the model pair and the trigger. A fallback with no message ordinal is skipped in
// the transcript (it has no turn to mark) but still appears in the tile tooltip.
func TestTranscriptFallbackNotice(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	d.ModelFallbackCount = 2
	hs := HeaderStats{Fallbacks: []store.ModelFallback{
		{
			MessageOrdinal: intPtr(1),
			FromModel:      "claude-fable-5",
			ToModel:        "claude-opus-4-8",
			Trigger:        "reasoning_extraction",
		},
		{
			// No ordinal: it has no turn to mark, so it appears only in the tooltip.
			FromModel: "claude-fable-5",
			ToModel:   "claude-opus-4-8",
			DedupKey:  "orphan",
		},
	}}
	html := renderComponent(t, SessionPage(p, d, msgs, tools, nil, nil, hs, 0, false, true))

	if !strings.Contains(html, `class="msg-fallback"`) {
		t.Error("the transcript should mark the fallback turn with an inline notice")
	}
	if !strings.Contains(html, "Fell back from claude-fable-5 to claude-opus-4-8 (reasoning_extraction)") {
		t.Error("the notice should name the model pair and the trigger")
	}
	// The tile count still reflects both fallbacks even though only one marks a turn.
	if !strings.Contains(html, `data-stat-key="fallbacks">2</div>`) {
		t.Error("the tile count should include a fallback that marks no turn")
	}
}

// FallbacksByOrdinal indexes by turn, drops a fallback with no ordinal, and keeps the
// first when two share a turn (the slice arrives ordered by occurrence).
func TestFallbacksByOrdinal(t *testing.T) {
	fbs := []store.ModelFallback{
		{MessageOrdinal: intPtr(3), FromModel: "first"},
		{MessageOrdinal: intPtr(3), FromModel: "second"}, // same turn, later
		{FromModel: "no-ordinal"},                        // dropped
	}
	m := FallbacksByOrdinal(fbs)
	if len(m) != 1 {
		t.Fatalf("map = %d entries, want 1", len(m))
	}
	if m[3].FromModel != "first" {
		t.Errorf("first fallback should win the slot, got %q", m[3].FromModel)
	}
	if FallbacksByOrdinal(nil) != nil {
		t.Error("no fallbacks should yield a nil map")
	}
}

// FallbackDeclinedTokens sums only the classes that were measured and returns "" when
// none were, so the caller omits the row rather than showing a misleading zero.
func TestFallbackDeclinedTokens(t *testing.T) {
	if got := FallbackDeclinedTokens(store.ModelFallback{}); got != "" {
		t.Errorf("no measured spend = %q, want empty", got)
	}
	f := store.ModelFallback{DeclinedInput: int64Ptr(1000), DeclinedCacheRead: int64Ptr(2000)}
	if got := FallbackDeclinedTokens(f); got != "3,000" {
		t.Errorf("summed spend = %q, want 3,000", got)
	}
	// A single measured zero still counts as measured, so it reads "0" not empty.
	if got := FallbackDeclinedTokens(store.ModelFallback{DeclinedOutput: int64Ptr(0)}); got != "0" {
		t.Errorf("a measured zero = %q, want 0", got)
	}
}

// The fallback notice names the refusal category, falls back to the trigger when the
// category is unset, and omits the parenthetical when neither is known.
func TestFallbackNoticeLabel(t *testing.T) {
	both := store.ModelFallback{FromModel: "a", ToModel: "b", Trigger: "t", RefusalCategory: "c"}
	if got := FallbackNoticeLabel(both); got != "Fell back from a to b (c)" {
		t.Errorf("both set = %q, want the category preferred", got)
	}
	triggerOnly := store.ModelFallback{FromModel: "a", ToModel: "b", Trigger: "t"}
	if got := FallbackNoticeLabel(triggerOnly); got != "Fell back from a to b (t)" {
		t.Errorf("trigger fallback = %q", got)
	}
	catOnly := store.ModelFallback{FromModel: "a", ToModel: "b", RefusalCategory: "c"}
	if got := FallbackNoticeLabel(catOnly); got != "Fell back from a to b (c)" {
		t.Errorf("category only = %q", got)
	}
	bare := store.ModelFallback{}
	if got := FallbackNoticeLabel(bare); got != "Fell back from unknown to unknown" {
		t.Errorf("bare = %q", got)
	}
}
