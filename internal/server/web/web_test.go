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

// plural switches a count's suffix so the remainder footer reads "1 session" but
// "2 sessions" without each call site repeating the conditional.
func TestPlural(t *testing.T) {
	if got := plural(1); got != "" {
		t.Errorf("plural(1) = %q, want empty", got)
	}
	for _, n := range []int{0, 2, 5} {
		if got := plural(n); got != "s" {
			t.Errorf("plural(%d) = %q, want \"s\"", n, got)
		}
	}
}

// TestProjectSessionListRemainderFooter renders the per-project table with and
// without a withheld tail and confirms the reconciling footer appears only when
// sessions were capped out. Crucially it renders the hidden tail's token volume
// through the shared token card (the same four-class breakdown every other token
// figure carries), not a bare number, so the footer reads consistently and the
// reader can see what the hidden sessions spent.
func TestProjectSessionListRemainderFooter(t *testing.T) {
	rows := []store.SessionSummary{{ID: 1, Agent: "claude", TotalInput: 300, TotalOutput: 100, TotalCostUSD: 4.00}}

	rem := store.SessionRemainder{
		Sessions: 5, Input: 400, Output: 200, CacheRead: 100, CacheWrite: 50, CostUSD: 5.50,
	}
	withTail := renderComponent(t, ProjectSessionList(rows, rem))
	if !strings.Contains(withTail, "+5 more sessions in this range") {
		t.Error("footer should summarize the withheld sessions when a remainder exists")
	}
	if !strings.Contains(withTail, "<tfoot>") {
		t.Error("remainder should render as a table footer")
	}
	// The footer token figure goes through the shared card: a tok-cell wrapping the
	// per-class breakdown, the same treatment every other token figure gets.
	foot := withTail[strings.Index(withTail, "<tfoot>"):]
	for _, want := range []string{`class="tok-cell"`, "tok-tip", "<dt>Cache read</dt>", "750 tokens"} {
		if !strings.Contains(foot, want) {
			t.Errorf("remainder footer should carry the shared token card, missing %q", want)
		}
	}

	none := renderComponent(t, ProjectSessionList(rows, store.SessionRemainder{}))
	if strings.Contains(none, "<tfoot>") || strings.Contains(none, "more session") {
		t.Error("no footer should render when every windowed session is shown")
	}

	// A single withheld session reads in the singular.
	one := renderComponent(t, ProjectSessionList(rows, store.SessionRemainder{Sessions: 1, Input: 10}))
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
