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

// A feed row shows the fan-out chip only when the session spawned subagents, carrying the
// count and the whole-work-item cost, with the "row's own cost" caveat in its title. A row
// that fanned out nothing draws no chip, so its absence reads as "no fan-out".
func TestGlobalSessionListFanoutChip(t *testing.T) {
	ts := time.Now().UTC().Add(-2 * 24 * time.Hour)
	rows := []store.SessionRow{{
		SessionSummary: store.SessionSummary{
			ID: 12, Agent: "claude", GitBranch: "main", Username: "ada",
			MessageCount: 9, LastActiveAt: &ts,
		},
		ProjectID: 3, ProjectKey: "akari", ProjectName: "akari", ProjectKind: "remote",
		Tree: store.TreeRollup{SubagentCount: 62, CostUSD: 4.12},
	}}
	html := renderComponent(t, GlobalSessionList(rows, store.SessionFilter{Sort: "updated", Desc: true}, SessionFooter{Shown: 1}))
	for _, want := range []string{
		`class="tag fanout"`,
		`62 subagents · $4.12</span>`,
		`the row&#39;s own cost is the root turn&#39;s alone`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("feed row missing fan-out chip %q", want)
		}
	}

	// A session that spawned nothing (zero-value rollup) draws no chip.
	rows[0].Tree = store.TreeRollup{}
	bare := renderComponent(t, GlobalSessionList(rows, store.SessionFilter{Sort: "updated", Desc: true}, SessionFooter{Shown: 1}))
	if strings.Contains(bare, "tag fanout") {
		t.Error("a session with no subagents should draw no fan-out chip")
	}
}

// The header renders the Fallbacks tile only when the session had a fallback, and its
// tooltip lists each one: the model pair (a real arrow glyph), the refusal category, the
// declined attempt's token spend broken out by class, and the time. A session with no
// fallback draws no tile, so its absence is the negative signal.
func TestSessionStatsFallbackTile(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	d.ModelFallbackCount = 1
	when := time.Date(2026, 6, 28, 9, 5, 0, 0, time.UTC)
	hs := HeaderStats{Fallbacks: []store.ModelFallback{{
		MessageOrdinal:     intPtr(1),
		FromModel:          "claude-fable-5",
		ToModel:            "claude-opus-4-8",
		Trigger:            "reasoning_extraction",
		RefusalCategory:    "reasoning_extraction",
		DeclinedInput:      int64Ptr(1200),
		DeclinedOutput:     int64Ptr(300),
		DeclinedCacheWrite: int64Ptr(1500),
		DeclinedCacheRead:  int64Ptr(5000),
		OccurredAt:         &when,
	}}}
	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, hs, 0), false, true))

	for _, want := range []string{
		`class="stat fallback-stat"`,
		`data-stat-key="fallbacks">1</div>`,
		`>Models</dt>`, `<dd>claude-fable-5 → claude-opus-4-8</dd>`, // the model pair, arrow glyph
		`>Category</dt>`, `<dd>reasoning_extraction</dd>`,
		`>Declined input</dt>`, `<dd>1,200</dd>`, // each class on its own row, not one summed figure
		`>Declined output</dt>`, `<dd>300</dd>`,
		`>Declined cache write</dt>`, `<dd>1,500</dd>`,
		`>Declined cache read</dt>`, `<dd>5,000</dd>`,
		`>Time</dt>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("fallback tile missing %q", want)
		}
	}

	// A session with no fallback draws no tile at all: absence is the signal.
	d.ModelFallbackCount = 0
	bare := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, HeaderStats{}, 0), false, true))
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
	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, hs, 0), false, true))

	if !strings.Contains(html, `<dd>unknown → unknown</dd>`) {
		t.Error("an unnamed model pair should read unknown, not blank")
	}
	if !strings.Contains(html, `<dd>uncategorized</dd>`) {
		t.Error("an empty refusal category should read uncategorized")
	}
	if !strings.Contains(html, `<dd>-</dd>`) {
		t.Error("a fallback with no timestamp should read a dash")
	}
	if strings.Contains(html, `>Declined input</dt>`) {
		t.Error("a fallback with no measured spend should omit the declined-token rows")
	}
}

// A fallback whose declined spend is only partly measured (a nil class) shows no declined
// rows at all: the classes merge in together, so a partial breakdown would mislead. The
// tooltip skips the whole group rather than reporting some classes and a phantom zero for
// the rest.
func TestFallbackTilePartialDeclinedOmitted(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()
	d.ModelFallbackCount = 1
	// Three of four classes measured, cache read still nil: the group must not render.
	hs := HeaderStats{Fallbacks: []store.ModelFallback{{
		DedupKey:           "partial",
		DeclinedInput:      int64Ptr(100),
		DeclinedOutput:     int64Ptr(200),
		DeclinedCacheWrite: int64Ptr(300),
	}}}
	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, hs, 0), false, true))
	if strings.Contains(html, `>Declined input</dt>`) {
		t.Error("a partly measured declined spend should omit the declined-token rows entirely")
	}
}

// The tooltip lists at most the first five fallbacks and names the remainder in a closing
// "plus N more" line computed from the session count, so a session that fell back many times
// keeps a short tooltip. A session at or under five shows every one and no overflow line.
func TestFallbackTileCapsListAndCountsOverflow(t *testing.T) {
	p := Page{Title: "session", LoggedIn: true, Active: "sessions", Username: "jessoteric"}
	d, msgs, tools := sessionFixture()

	// Eight fetched rows against a session-wide count of twelve: five render, and the note
	// reports twelve minus five shown, not eight minus five fetched.
	d.ModelFallbackCount = 12
	var many []store.ModelFallback
	for i := 0; i < 8; i++ {
		many = append(many, store.ModelFallback{FromModel: "claude-fable-5", ToModel: "claude-opus-4-8", DedupKey: string(rune('a' + i))})
	}
	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, HeaderStats{Fallbacks: many}, 0), false, true))
	if got := strings.Count(html, `>Models</dt>`); got != 5 {
		t.Errorf("tooltip rendered %d fallback rows, want 5", got)
	}
	if !strings.Contains(html, `plus 7 more`) {
		t.Error("tooltip should note the remainder as 'plus 7 more' (count 12 minus 5 shown)")
	}

	// A session with three fallbacks shows all three and no overflow note.
	d.ModelFallbackCount = 3
	few := many[:3]
	small := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, HeaderStats{Fallbacks: few}, 0), false, true))
	if got := strings.Count(small, `>Models</dt>`); got != 3 {
		t.Errorf("tooltip rendered %d fallback rows, want 3", got)
	}
	if strings.Contains(small, `plus `) {
		t.Error("a session under the cap should show no overflow note")
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
	html := renderComponent(t, SessionPage(p, viewFor(d, msgs, tools, nil, hs, 0), false, true))

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

// FallbackDeclinedObserved is true only when every token class merged in: the classes
// arrive together, so a single nil means the spend was never observed and the tooltip omits
// the whole per-class breakdown rather than showing a partial, misleading one.
func TestFallbackDeclinedObserved(t *testing.T) {
	if FallbackDeclinedObserved(store.ModelFallback{}) {
		t.Error("no measured classes should not read as observed")
	}
	// Three of four present is still not observed: the missing class breaks the breakdown.
	partial := store.ModelFallback{DeclinedInput: int64Ptr(1), DeclinedOutput: int64Ptr(2), DeclinedCacheWrite: int64Ptr(3)}
	if FallbackDeclinedObserved(partial) {
		t.Error("a partly measured spend should not read as observed")
	}
	// All four present (measured zeros count) reads as observed.
	full := store.ModelFallback{DeclinedInput: int64Ptr(0), DeclinedOutput: int64Ptr(0), DeclinedCacheWrite: int64Ptr(0), DeclinedCacheRead: int64Ptr(0)}
	if !FallbackDeclinedObserved(full) {
		t.Error("all four classes measured (even zeros) should read as observed")
	}
}

// FallbacksOverflow is the session count beyond what the tooltip shows, never negative: it
// drives the "plus N more" line when the list was capped and is zero when the shown rows
// already cover the count.
func TestFallbacksOverflow(t *testing.T) {
	if got := FallbacksOverflow(12, 5); got != 7 {
		t.Errorf("overflow(12, 5) = %d, want 7", got)
	}
	if got := FallbacksOverflow(3, 5); got != 0 {
		t.Errorf("overflow(3, 5) = %d, want 0 (shown covers the count)", got)
	}
	if got := FallbacksOverflow(5, 5); got != 0 {
		t.Errorf("overflow(5, 5) = %d, want 0", got)
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
