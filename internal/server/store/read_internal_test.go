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

	// An empty or unknown Sort is the descending updated feed order, id descending.
	for _, sort := range []string{"", "nonsense", "; drop table sessions"} {
		got := SessionFilter{Sort: sort}.orderClause()
		if want := " ORDER BY s.updated_at DESC, s.id DESC"; got != want {
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
	short := buildSnippet("Fix the timezone pass", "TIMEZONE")
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
	snip := buildSnippet(long, "needle")
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

	// A content that does not contain the query yields a zero snippet rather than a
	// wrong window (the caller only builds one for a matched row, but be safe).
	if none := buildSnippet("nothing here", "absent"); none.Has() {
		t.Errorf("no-match content should yield an empty snippet, got %q", none.Text)
	}
}

// TestSquashSpaces collapses whitespace runs to single spaces and trims the ends.
func TestSquashSpaces(t *testing.T) {
	t.Parallel()
	if got := squashSpaces("  a\n\n b\t c  "); got != "a b c" {
		t.Errorf("squashSpaces = %q, want 'a b c'", got)
	}
}
