package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

// TestSessionsQueryRoundTrip pins that every filter field the session list carries
// round-trips through sessionsQuery/SessionsPath: a filter with q, grade, outcome,
// range, empty, and a grown limit all serialize, and the encoding stays stable
// (sorted keys) so a swap target and a facet link agree byte for byte.
func TestSessionsQueryRoundTrip(t *testing.T) {
	f := store.SessionFilter{
		Query:        "refactor pricing",
		Grade:        "A",
		Outcome:      "completed",
		Range:        "30d",
		IncludeEmpty: true,
		Limit:        200,
	}
	got := SessionsPath(f)
	if !strings.HasPrefix(got, SessionsBasePath+"?") {
		t.Fatalf("SessionsPath = %q, want a query-carrying /sessions path", got)
	}
	q, err := url.ParseQuery(strings.TrimPrefix(got, SessionsBasePath+"?"))
	if err != nil {
		t.Fatalf("parse round-trip query: %v", err)
	}
	for key, want := range map[string]string{
		"q":       "refactor pricing",
		"grade":   "A",
		"outcome": "completed",
		"range":   "30d",
		"empty":   "1",
		"limit":   "200",
	} {
		if got := q.Get(key); got != want {
			t.Errorf("round-trip %s = %q, want %q (full path %q)", key, got, want, got)
		}
	}

	// The default page size is NOT serialized (the bare limit is implied), so a
	// first-page filter stays a clean path.
	if got := SessionsPath(store.SessionFilter{Limit: DefaultSessionLimit}); got != SessionsBasePath {
		t.Errorf("default-limit path = %q, want the bare /sessions", got)
	}
	// A bare filter is the bare path.
	if got := SessionsPath(store.SessionFilter{}); got != SessionsBasePath {
		t.Errorf("empty filter path = %q, want the bare /sessions", got)
	}
}

// TestAnyFilterActiveDrillBranches covers the branches AnyFilterActive gained for the
// drill-through fields: grade, outcome, and range each count as an active narrowing so
// the "clear all" affordance shows for a drill-through feed, not only a facet one.
func TestAnyFilterActiveDrillBranches(t *testing.T) {
	for _, f := range []store.SessionFilter{
		{Query: "x"}, {Grade: "A"}, {Outcome: "abandoned"}, {Range: "7d"},
	} {
		if !AnyFilterActive(f) {
			t.Errorf("filter %+v should read as active", f)
		}
	}
	// A limit or sort alone is not a narrowing: it is paging/order, not a filter, so it
	// does not trip the clear-all affordance.
	if AnyFilterActive(store.SessionFilter{Limit: 400, Sort: "tokens", Desc: true}) {
		t.Error("limit and sort alone should not read as an active filter")
	}
}

// TestClearHrefsDropOnlyTheirField pins that each chip's clear link drops exactly its
// own field and holds the rest, and that the two that reset paging (search, empty) do
// so while grade/outcome/range leave the page size alone.
func TestClearHrefsDropOnlyTheirField(t *testing.T) {
	// A filter with every field set, plus a grown page, so a clear that wrongly touched
	// another field would show in the round-trip.
	base := store.SessionFilter{
		Agent: "claude", Username: "ada", Query: "q",
		Grade: "A", Outcome: "completed", Range: "30d",
		IncludeEmpty: true, Limit: 200,
	}
	// GradeClearHref drops grade, keeps outcome, range, and the grown limit.
	g := mustQuery(t, string(GradeClearHref(base)))
	if g.Get("grade") != "" || g.Get("outcome") != "completed" || g.Get("range") != "30d" || g.Get("limit") != "200" {
		t.Errorf("GradeClearHref should drop only grade, got %v", g)
	}
	// OutcomeClearHref drops outcome, keeps grade.
	o := mustQuery(t, string(OutcomeClearHref(base)))
	if o.Get("outcome") != "" || o.Get("grade") != "A" {
		t.Errorf("OutcomeClearHref should drop only outcome, got %v", o)
	}
	// RangeClearHref drops range, keeps grade and outcome.
	r := mustQuery(t, string(RangeClearHref(base)))
	if r.Get("range") != "" || r.Get("grade") != "A" || r.Get("outcome") != "completed" {
		t.Errorf("RangeClearHref should drop only range, got %v", r)
	}
	// SearchClearHref drops q AND resets the page (the expanded limit was scoped to the
	// search results), while holding the facets and drill fields.
	s := mustQuery(t, string(SearchClearHref(base)))
	if s.Get("q") != "" || s.Get("limit") != "" || s.Get("grade") != "A" || s.Get("agent") != "claude" {
		t.Errorf("SearchClearHref should drop q and limit, hold the rest, got %v", s)
	}
	// EmptyToggleHref flips empty and resets the page (the visible count changes), while
	// holding the facets and drill fields.
	e := mustQuery(t, string(EmptyToggleHref(base)))
	if e.Get("empty") != "" || e.Get("limit") != "" || e.Get("grade") != "A" {
		t.Errorf("EmptyToggleHref from shown should drop empty and reset limit, got %v", e)
	}
	// From a hidden state the toggle turns empties on.
	on := mustQuery(t, string(EmptyToggleHref(store.SessionFilter{Agent: "claude"})))
	if on.Get("empty") != "1" || on.Get("agent") != "claude" {
		t.Errorf("EmptyToggleHref from hidden should set empty=1 and hold the facet, got %v", on)
	}
}

// TestNextSessionLimit pins the doubling and clamp: an unset limit starts at the
// default, doubles toward the cap, and never exceeds it.
func TestNextSessionLimit(t *testing.T) {
	if got := NextSessionLimit(0); got != DefaultSessionLimit*2 {
		t.Errorf("NextSessionLimit(0) = %d, want the default doubled %d", got, DefaultSessionLimit*2)
	}
	if got := NextSessionLimit(DefaultSessionLimit); got != DefaultSessionLimit*2 {
		t.Errorf("NextSessionLimit(default) = %d, want %d", got, DefaultSessionLimit*2)
	}
	if got := NextSessionLimit(300); got != MaxSessionLimit {
		t.Errorf("NextSessionLimit(300) = %d, want the cap %d (600 clamps down)", got, MaxSessionLimit)
	}
	if got := NextSessionLimit(MaxSessionLimit); got != MaxSessionLimit {
		t.Errorf("NextSessionLimit(cap) = %d, want to stay at the cap", got)
	}
}

// TestSessionSortOptionsHasCost pins that the sort control offers the cost order the
// store gained, so the "Most expensive" menu item is reachable.
func TestSessionSortOptionsHasCost(t *testing.T) {
	var found bool
	for _, o := range SessionSortOptions() {
		if o.Key == "cost" {
			found = true
		}
	}
	if !found {
		t.Error("SessionSortOptions should offer the cost order")
	}
}

// TestSplitSnippetMalformed pins the defensive path: out-of-range or inverted offsets
// collapse the whole text into the Before run with an empty match, so a bad window
// degrades to plain text rather than panicking on a slice out of bounds.
func TestSplitSnippetMalformed(t *testing.T) {
	text := "hello world"
	for _, s := range []store.SearchSnippet{
		{Text: text, MatchStart: -1, MatchEnd: 3},         // negative start
		{Text: text, MatchStart: 2, MatchEnd: 999},        // end past the text
		{Text: text, MatchStart: 8, MatchEnd: 3},          // start after end
	} {
		parts := SplitSnippet(s)
		if parts.Before != text || parts.Match != "" || parts.After != "" {
			t.Errorf("malformed offsets %+v should collapse to plain Before text, got %+v", s, parts)
		}
	}
	// A well-formed snippet splits into the three runs.
	ok := SplitSnippet(store.SearchSnippet{Text: "abcXYZdef", MatchStart: 3, MatchEnd: 6})
	if ok.Before != "abc" || ok.Match != "XYZ" || ok.After != "def" {
		t.Errorf("well-formed split = %+v, want abc/XYZ/def", ok)
	}
}

// mustQuery parses the query portion of a path, failing the test on a malformed one.
func mustQuery(t *testing.T, path string) url.Values {
	t.Helper()
	i := strings.IndexByte(path, '?')
	if i < 0 {
		return url.Values{}
	}
	v, err := url.ParseQuery(path[i+1:])
	if err != nil {
		t.Fatalf("parse query of %q: %v", path, err)
	}
	return v
}
