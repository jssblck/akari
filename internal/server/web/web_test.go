package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// TestSessionPageTitle pins the browser-tab title for a session view: the
// session's own summary wins when it has one, and an untitled session falls back
// to a stable "<project> session" label (the folder name for a local project, the
// remote key otherwise), never a bare id.
func TestSessionPageTitle(t *testing.T) {
	titled := store.SessionDetail{SessionSummary: store.SessionSummary{Title: "Fix the timezone pass"}, ProjectKey: "akari", ProjectKind: "remote"}
	if got := SessionPageTitle(titled); got != "Fix the timezone pass" {
		t.Errorf("titled session = %q, want the session title", got)
	}
	remote := store.SessionDetail{ProjectKey: "github.com/jssblck/akari", ProjectKind: "remote"}
	if got := SessionPageTitle(remote); got != "github.com/jssblck/akari session" {
		t.Errorf("untitled remote session = %q", got)
	}
	local := store.SessionDetail{ProjectName: "akari", ProjectKey: "local:hopper:/src/akari", ProjectKind: "standalone"}
	if got := SessionPageTitle(local); got != "akari session" {
		t.Errorf("untitled local session = %q, want the folder name, not the synthetic key", got)
	}
}

// TestErrorTitle pins the public error page's tab title: a known status pairs the
// code with its standard reason, and an unknown code degrades to the number alone.
func TestErrorTitle(t *testing.T) {
	if got := errorTitle(404); got != "404 Not Found" {
		t.Errorf("404 title = %q", got)
	}
	if got := errorTitle(499); got != "499" {
		t.Errorf("unknown-code title = %q, want the bare number", got)
	}
}

// TestDetailLabel pins the chip-summary rendering for store.ToolCallView.Detail:
// whitespace of any kind collapses to single spaces so a multi-line shell
// command reads as one scannable line, and the output is capped at 80 runes
// with a trailing ellipsis so a chip never grows to the size of its input. The
// full text still reaches the reader through the element's title attribute, so
// the cap here is purely a display concern.
func TestDetailLabel(t *testing.T) {
	if got := DetailLabel(""); got != "" {
		t.Errorf("empty detail = %q, want empty", got)
	}
	if got := DetailLabel("go test ./..."); got != "go test ./..." {
		t.Errorf("plain detail = %q", got)
	}
	// A multi-line command (tabs, newlines, repeated spaces) collapses to one
	// space-separated line, the same rendering an OutlineTitle turn gets.
	if got := DetailLabel("go build \\\n\t./...  &&\ngo test ./..."); got != "go build \\ ./... && go test ./..." {
		t.Errorf("multiline detail = %q", got)
	}
	// Content beyond the cap is truncated with a single trailing ellipsis rune
	// and never longer than the cap plus that rune, regardless of input size.
	long := ""
	for i := 0; i < 40; i++ {
		long += "word "
	}
	got := DetailLabel(long)
	runes := []rune(got)
	if runes[len(runes)-1] != '…' {
		t.Errorf("a long detail should end with an ellipsis: %q", got)
	}
	if n := len(runes); n != 81 {
		t.Errorf("a long detail should be exactly cap+ellipsis runes, got %d: %q", n, got)
	}
	// A multi-byte rune sitting right at the 80-rune boundary must be emitted
	// whole, never split mid-sequence: the cap counts runes, not bytes.
	boundary := strings.Repeat("a", 79) + "€word"
	got = DetailLabel(boundary)
	if !strings.HasPrefix(got, strings.Repeat("a", 79)+"€") {
		t.Errorf("a multi-byte rune at the boundary should be emitted intact: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("truncation past the boundary rune should still end in an ellipsis: %q", got)
	}
}

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

	html := renderComponent(t, SessionPage(p, d, msgs, FullTranscript(msgs), tools, nil, nil, HeaderStats{}, 1, false, false))
	if !strings.Contains(html, "1 duplicate id") {
		t.Error("session page should show the duplicate-id chip when the count is non-zero")
	}

	html = renderComponent(t, SessionPage(p, d, msgs, FullTranscript(msgs), tools, nil, nil, HeaderStats{}, 0, false, false))
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
// absolute stamp once a relative one stops being useful. Day distance is measured in
// the viewer's zone, so the same instant can bucket differently depending on the
// reader's timezone.
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
			if got := relTime(now, c.when, time.UTC); got != c.want {
				t.Errorf("relTime(%s) = %q, want %q", c.when.Format(time.RFC3339), got, c.want)
			}
		})
	}

	// A viewer's zone can move an instant across the day boundary, changing the
	// bucket. At 2026-06-29 03:00 UTC a stamp from 2026-06-29 01:00 UTC is "today" in
	// UTC, but a viewer in a zone eight hours behind (US Pacific, PDT) is still on
	// 2026-06-28 for both instants at their local wall clock: 03:00 UTC is 2026-06-28
	// 20:00 local, so it reads "today" there too. Shift the reference clock forward to
	// local midnight to make the two zones disagree.
	pacific, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("America/Los_Angeles unavailable: %v", err)
	}
	// 2026-06-29 07:30 UTC is 2026-06-29 00:30 PDT (just past local midnight), while
	// the stamp 2026-06-29 05:00 UTC is 2026-06-28 22:00 PDT (still the day before).
	nowUTC := time.Date(2026, 6, 29, 7, 30, 0, 0, time.UTC)
	stamp := time.Date(2026, 6, 29, 5, 0, 0, 0, time.UTC)
	if got := relTime(nowUTC, stamp, time.UTC); got != "today" {
		t.Errorf("in UTC both instants sit on 2026-06-29, want %q, got %q", "today", got)
	}
	if got := relTime(nowUTC, stamp, pacific); got != "1 day ago" {
		t.Errorf("in Pacific the stamp is the previous local day, want %q, got %q", "1 day ago", got)
	}
}

// FmtRelTime returns a dash for the absent timestamp rather than panicking on a
// nil pointer or formatting the zero time.
func TestFmtRelTimeAbsent(t *testing.T) {
	ctx := context.Background()
	if got := FmtRelTime(ctx, nil); got != "-" {
		t.Errorf("FmtRelTime(nil) = %q, want %q", got, "-")
	}
	var zero time.Time
	if got := FmtRelTime(ctx, &zero); got != "-" {
		t.Errorf("FmtRelTime(zero) = %q, want %q", got, "-")
	}
}

// FmtTime and FmtTimeAt render in the viewer's zone carried on the context, and
// FmtTimeLong appends the zone abbreviation so a hover title names its zone.
func TestFmtTimeLocalizes(t *testing.T) {
	pacific, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("America/Los_Angeles unavailable: %v", err)
	}
	// 2026-06-29 07:30 UTC is 2026-06-29 00:30 PDT.
	ts := time.Date(2026, 6, 29, 7, 30, 0, 0, time.UTC)
	utcCtx := context.Background()
	laCtx := WithLoc(context.Background(), pacific)

	if got := FmtTime(utcCtx, &ts); got != "2026-06-29 07:30" {
		t.Errorf("FmtTime UTC = %q, want %q", got, "2026-06-29 07:30")
	}
	if got := FmtTime(laCtx, &ts); got != "2026-06-29 00:30" {
		t.Errorf("FmtTime Pacific = %q, want %q", got, "2026-06-29 00:30")
	}
	if got := FmtTimeAt(laCtx, ts); got != "2026-06-29 00:30" {
		t.Errorf("FmtTimeAt Pacific = %q, want %q", got, "2026-06-29 00:30")
	}
	if got := FmtTimeLong(utcCtx, &ts); got != "2026-06-29 07:30 UTC" {
		t.Errorf("FmtTimeLong UTC = %q, want %q", got, "2026-06-29 07:30 UTC")
	}
	if got := FmtTimeLong(laCtx, &ts); got != "2026-06-29 00:30 PDT" {
		t.Errorf("FmtTimeLong Pacific = %q, want %q", got, "2026-06-29 00:30 PDT")
	}
}

// TestFmtTokensCompact pins the magnitude buckets, which must mirror fmtTok in
// static/charts.js so a figure reads the same server- and client-rendered. The
// billions case exists for parity with the JS side even though today's sole
// caller (the feed row) is unlikely to reach it.
func TestFmtTokensCompact(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{412, "412"},
		{63_049, "63.0k"},
		{1_700_000, "1.7M"},
		{999_999_999, "1000.0M"},
		{1_500_000_000, "1.5B"},
	}
	for _, c := range cases {
		if got := FmtTokensCompact(c.n); got != c.want {
			t.Errorf("FmtTokensCompact(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// TestFmtCost pins the dollar rendering at every magnitude: two decimals from a
// cent up with no whole-dollar rounding at any size, four decimals below a cent,
// and the incomplete marker appended. fmtCost in static/charts.js mirrors these
// rules; a change here needs the same change there.
func TestFmtCost(t *testing.T) {
	cases := []struct {
		usd        float64
		incomplete bool
		want       string
	}{
		{0, false, "$0"},
		{0.0042, false, "$0.0042"},
		{3.5, false, "$3.50"},
		{1234.567, false, "$1234.57"},
		{12.3, true, "$12.30+"},
	}
	for _, c := range cases {
		if got := FmtCost(c.usd, c.incomplete); got != c.want {
			t.Errorf("FmtCost(%v, %v) = %q, want %q", c.usd, c.incomplete, got, c.want)
		}
	}
}
