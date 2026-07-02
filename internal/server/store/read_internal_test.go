package store

import (
	"strings"
	"testing"
)

// TestOrderClause locks the two properties that let the global session list walk
// an index instead of sorting the whole history: the ORDER BY carries no NULLS
// placement (which would defeat the index match, since every sortable expression
// is NOT NULL) and the id tiebreak follows the column's direction (so one (col,
// id) btree serves both the ascending and descending header clicks). It is a
// white-box guard against silently reintroducing "NULLS LAST", which once defeated
// even the default feed order.
func TestOrderClause(t *testing.T) {
	t.Parallel()

	// An empty or unknown Sort is the descending recency feed order (last_active_at,
	// the session's last-event time, not the row's updated_at write time), id descending.
	for _, sort := range []string{"", "nonsense", "; drop table sessions"} {
		got := SessionFilter{Sort: sort}.orderClause()
		if want := " ORDER BY s.last_active_at DESC, s.id DESC"; got != want {
			t.Errorf("default orderClause(sort=%q) = %q, want %q", sort, got, want)
		}
	}

	for key, expr := range sessionSortColumns {
		for _, desc := range []bool{false, true} {
			got := SessionFilter{Sort: key, Desc: desc}.orderClause()
			if strings.Contains(got, "NULLS") {
				t.Errorf("orderClause(%q, desc=%v) = %q: must not place NULLS (it defeats the index)", key, desc, got)
			}
			dir := "ASC"
			if desc {
				dir = "DESC"
			}
			if want := " ORDER BY " + expr + " " + dir + ", s.id " + dir; got != want {
				t.Errorf("orderClause(%q, desc=%v) = %q, want %q", key, desc, got, want)
			}
		}
	}
}

// TestLikePattern locks the escaping that keeps a user's search a literal
// substring: the LIKE metacharacters (%, _, and the escape backslash) are escaped
// so a query containing one matches itself rather than acting as a wildcard, and
// the whole thing is wrapped in the substring wildcards.
func TestLikePattern(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"pricing", `%pricing%`},
		{"95%", `%95\%%`},
		{"cache_read", `%cache\_read%`},
		{`a\b`, `%a\\b%`},
	}
	for _, c := range cases {
		if got := likePattern(c.in); got != c.want {
			t.Errorf("likePattern(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBuildSnippet windows a matching message's content around the first match,
// trims to word boundaries with ellipses when cut, and returns byte offsets that
// bracket exactly the matched run within the returned text.
func TestBuildSnippet(t *testing.T) {
	t.Parallel()

	// A short content that contains the match is returned whole, no ellipses, with
	// the offsets bracketing the match. The search is case-insensitive.
	short := buildSnippet("Fix the timezone pass", "TIMEZONE", false)
	if short.Text != "Fix the timezone pass" {
		t.Errorf("short snippet text = %q, want the whole content", short.Text)
	}
	if got := short.Text[short.MatchStart:short.MatchEnd]; got != "timezone" {
		t.Errorf("short snippet match run = %q, want 'timezone'", got)
	}

	// A long content windows around the match: both ends are cut (leading and
	// trailing ellipsis), the window stays near the budget, and the offsets still
	// bracket the match within the returned (ellipsis-prefixed) text.
	long := strings.Repeat("alpha bravo charlie delta ", 20) + "NEEDLE " + strings.Repeat("echo foxtrot golf hotel ", 20)
	snip := buildSnippet(long, "needle", false)
	if !strings.HasPrefix(snip.Text, "…") || !strings.HasSuffix(snip.Text, "…") {
		t.Errorf("a match in the middle of a long content should be cut both ends: %q", snip.Text)
	}
	if got := strings.ToLower(snip.Text[snip.MatchStart:snip.MatchEnd]); got != "needle" {
		t.Errorf("long snippet match run = %q, want 'needle'", got)
	}
	// The window is bounded near the budget plus the two ellipses; well under the
	// full content length.
	if len(snip.Text) > snippetWindow+16 {
		t.Errorf("snippet window = %d bytes, want near the %d budget", len(snip.Text), snippetWindow)
	}

	// frontCut forces a leading ellipsis even when this window's own trimming starts
	// at the head: the caller (the SQL match window) already dropped content ahead of
	// it, so the snippet must not read as the message's start. A match near the front
	// of the passed content, which alone would leave no lead cut, still gets one.
	cut := buildSnippet("needle in the passed window", "needle", true)
	if !strings.HasPrefix(cut.Text, "…") {
		t.Errorf("frontCut should force a leading ellipsis, got %q", cut.Text)
	}
	if got := strings.ToLower(cut.Text[cut.MatchStart:cut.MatchEnd]); got != "needle" {
		t.Errorf("frontCut snippet match run = %q, want 'needle'", got)
	}

	// Whitespace reconciliation: a query with a whitespace run matches content with a
	// run because both are squashed to one canonical form before the search. "foo  bar"
	// (two spaces) against "say foo  bar now" (two spaces) yields a snippet whose match
	// run is the single-spaced "foo bar", so a row the SQL ILIKE matched on raw content
	// still highlights rather than silently showing no snippet.
	ws := buildSnippet("say foo  bar now", "foo  bar", false)
	if !ws.Has() {
		t.Fatal("a whitespace-run query should still match squashed content")
	}
	if got := strings.ToLower(ws.Text[ws.MatchStart:ws.MatchEnd]); got != "foo bar" {
		t.Errorf("whitespace-run match run = %q, want the squashed 'foo bar'", got)
	}

	// A query that is only whitespace squashes to empty and yields no snippet, rather
	// than matching everywhere.
	if blank := buildSnippet("some content", "   ", false); blank.Has() {
		t.Errorf("an all-whitespace query should yield no snippet, got %q", blank.Text)
	}

	// A content that does not contain the query yields a zero snippet rather than a
	// wrong window (the caller only builds one for a matched row, but be safe).
	if none := buildSnippet("nothing here", "absent", false); none.Has() {
		t.Errorf("no-match content should yield an empty snippet, got %q", none.Text)
	}
}

// TestBuildSnippetCapsInput asserts the defensive input cap bounds buildSnippet's
// work: an input far larger than the cap is truncated (rune-safe) before any
// normalization, so peak memory stays proportional to the window regardless of the
// caller. A match past the cap is therefore not found (the snippet is bounded, not the
// whole message), which is the point: the SQL window already put the match in range,
// and any other caller gets a bounded scan rather than an unbounded one.
func TestBuildSnippetCapsInput(t *testing.T) {
	t.Parallel()

	// A match sitting at the very front survives the cap and yields a bounded snippet;
	// the returned text is at most the cap-derived window, never the multi-kilobyte input.
	huge := "needle " + strings.Repeat("x", 100_000)
	snip := buildSnippet(huge, "needle", false)
	if !snip.Has() {
		t.Fatal("a match at the head should still be found under the cap")
	}
	if len(snip.Text) > snippetWindow+16 {
		t.Errorf("capped snippet = %d bytes, want bounded near the %d window", len(snip.Text), snippetWindow)
	}

	// A cap that would land mid-rune backs off to a rune boundary, so the truncated
	// text stays valid UTF-8: build an input whose byte at the cap is a continuation
	// byte and confirm the truncation does not split it.
	multibyte := strings.Repeat("é", snippetInputCap) // 2 bytes each, so the cap lands mid-rune
	if got := runeSafeCut(multibyte, snippetInputCap); got%2 != 0 {
		t.Errorf("runeSafeCut = %d, want an even (rune-boundary) offset for a 2-byte-rune string", got)
	}
}

// TestSquashSpaces collapses whitespace runs to single spaces and trims the ends.
func TestSquashSpaces(t *testing.T) {
	t.Parallel()
	if got := squashSpaces("  a\n\n b\t c  "); got != "a b c" {
		t.Errorf("squashSpaces = %q, want 'a b c'", got)
	}
}
