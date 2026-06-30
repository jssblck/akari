package web

import (
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

func TestDuplicateIDsLabel(t *testing.T) {
	if got := DuplicateIDsLabel(1); got != "1 duplicate id" {
		t.Errorf("label(1) = %q", got)
	}
	if got := DuplicateIDsLabel(3); got != "3 duplicate ids" {
		t.Errorf("label(3) = %q", got)
	}
}

// TestSessionPageShowsDuplicateIDChip renders the session header with a non-zero and
// a zero duplicate-id count and confirms the warning chip appears only for the
// former, so a reader is told a session replayed an id (and a malformed reuse would
// not stay hidden). The count is a scalar the handler passes in, computed in the
// store rather than over the in-memory tool calls.
func TestSessionPageShowsDuplicateIDChip(t *testing.T) {
	p := Page{Title: "Session", LoggedIn: true, Username: "Grace Hopper"}
	d := store.SessionDetail{}
	d.ID = 7
	d.Agent = "claude"
	msgs := []store.Message{{Ordinal: 0, Role: "assistant"}, {Ordinal: 1, Role: "assistant"}}
	tools := map[int][]store.ToolCallView{
		0: {{MessageOrdinal: 0, ToolName: "Read"}},
		1: {{MessageOrdinal: 1, ToolName: "Read"}},
	}

	html := renderComponent(t, SessionPage(p, d, msgs, tools, nil, nil, 1, false, false))
	if !strings.Contains(html, "1 duplicate id") {
		t.Error("session page should show the duplicate-id chip when the count is non-zero")
	}

	html = renderComponent(t, SessionPage(p, d, msgs, tools, nil, nil, 0, false, false))
	if strings.Contains(html, "duplicate id") {
		t.Error("session page should not show the chip when the count is zero")
	}
}

// RowTokens sums every token class so the Tokens column and its breakdown card
// report the same total the heatmap does.
func TestRowTokens(t *testing.T) {
	s := store.SessionSummary{TotalInput: 100, TotalOutput: 50, TotalCacheRead: 7, TotalCacheWrite: 3}
	if got := RowTokens(s); got != 160 {
		t.Errorf("RowTokens = %d, want 160", got)
	}
}

// TestProjectSessionRemainder is the arithmetic that keeps the capped session table
// reconciled with the usage panel: the shown rows plus the remainder reproduce the
// panel totals exactly. It pins the three cases the project page relies on: a real
// withheld tail, a table that already shows everything (no footer), and cost
// rounding that must not surface as a negative remainder.
func TestProjectSessionRemainder(t *testing.T) {
	// Panel covers four sessions; the table shows two, so the footer carries the
	// other two and the difference of every figure.
	a := store.Analytics{Sessions: 4, TotalIn: 1000, TotalOut: 400, TotalCacheRead: 200, TotalCacheWrite: 100, TotalCost: 12.50}
	shown := []store.SessionSummary{
		{TotalInput: 300, TotalOutput: 100, TotalCacheRead: 50, TotalCacheWrite: 25, TotalCostUSD: 4.00},
		{TotalInput: 200, TotalOutput: 100, TotalCacheRead: 50, TotalCacheWrite: 25, TotalCostUSD: 3.00},
	}
	rem := ProjectSessionRemainder(a, shown)
	if !rem.Has() {
		t.Fatal("remainder should report a withheld tail when fewer rows show than the panel counts")
	}
	if rem.Sessions != 2 {
		t.Errorf("remainder sessions = %d, want 2", rem.Sessions)
	}
	// Headline tokens 1700 minus the shown 475 + 375 leaves 850; cost 12.50 minus
	// 7.00 leaves 5.50. Shown rows plus the footer reproduce the headline.
	if rem.Tokens != 850 {
		t.Errorf("remainder tokens = %d, want 850", rem.Tokens)
	}
	if rem.CostUSD != 5.50 {
		t.Errorf("remainder cost = %.2f, want 5.50", rem.CostUSD)
	}

	// When the table already shows every session, there is nothing to reconcile and
	// the footer stays absent.
	full := ProjectSessionRemainder(store.Analytics{Sessions: 2, TotalIn: 500, TotalCost: 4.0}, shown)
	if full.Has() {
		t.Errorf("remainder should be empty when all sessions show, got %+v", full)
	}

	// Float subtraction on cost must not leave a spurious negative once the session
	// count says nothing is withheld; the clamp keeps the footer honest.
	clamped := ProjectSessionRemainder(store.Analytics{Sessions: 2, TotalCost: 6.99}, shown)
	if clamped.Has() || clamped.CostUSD < 0 {
		t.Errorf("fully-shown table must report no remainder, got %+v", clamped)
	}
}

// TestProjectSessionListRemainderFooter renders the per-project table with and
// without a withheld tail and confirms the reconciling footer appears only when
// sessions were capped out, carrying the remainder count, tokens, and cost so the
// visible rows plus the footer read as the panel headline.
func TestProjectSessionListRemainderFooter(t *testing.T) {
	rows := []store.SessionSummary{{ID: 1, Agent: "claude", TotalInput: 300, TotalOutput: 100, TotalCostUSD: 4.00}}

	withTail := renderComponent(t, ProjectSessionList(rows, SessionRemainder{Sessions: 5, Tokens: 750, CostUSD: 5.50}))
	if !strings.Contains(withTail, "+5 more sessions in this range") {
		t.Error("footer should summarize the withheld sessions when a remainder exists")
	}
	if !strings.Contains(withTail, "<tfoot>") {
		t.Error("remainder should render as a table footer")
	}

	none := renderComponent(t, ProjectSessionList(rows, SessionRemainder{}))
	if strings.Contains(none, "<tfoot>") || strings.Contains(none, "more session") {
		t.Error("no footer should render when every windowed session is shown")
	}

	// A single withheld session reads in the singular.
	one := renderComponent(t, ProjectSessionList(rows, SessionRemainder{Sessions: 1, Tokens: 10}))
	if !strings.Contains(one, "+1 more session in this range") {
		t.Errorf("single withheld session should read in the singular, got footer text mismatch")
	}
}

// relTime buckets the recent past into coarse phrases and falls back to an
// absolute stamp once a relative one stops being useful.
func TestRelTime(t *testing.T) {
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		when time.Time
		want string
	}{
		{"same day earlier", time.Date(2026, 6, 29, 1, 0, 0, 0, time.UTC), "today"},
		{"late last night is a day ago", time.Date(2026, 6, 28, 23, 30, 0, 0, time.UTC), "1 day ago"},
		{"three days", time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC), "3 days ago"},
		{"six days", time.Date(2026, 6, 23, 8, 0, 0, 0, time.UTC), "6 days ago"},
		{"a week falls back to a stamp", time.Date(2026, 6, 22, 8, 0, 0, 0, time.UTC), "2026-06-22 08:00"},
		{"future clock skew reads today", time.Date(2026, 6, 29, 18, 0, 0, 0, time.UTC), "today"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := relTime(now, c.when); got != c.want {
				t.Errorf("relTime(%s) = %q, want %q", c.when.Format(time.RFC3339), got, c.want)
			}
		})
	}
}

// FmtRelTime returns a dash for the absent timestamp rather than panicking on a
// nil pointer or formatting the zero time.
func TestFmtRelTimeAbsent(t *testing.T) {
	if got := FmtRelTime(nil); got != "-" {
		t.Errorf("FmtRelTime(nil) = %q, want %q", got, "-")
	}
	var zero time.Time
	if got := FmtRelTime(&zero); got != "-" {
		t.Errorf("FmtRelTime(zero) = %q, want %q", got, "-")
	}
}
