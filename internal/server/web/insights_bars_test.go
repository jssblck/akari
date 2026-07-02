package web

import (
	"net/url"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

// hrefQuery parses the query portion of a drill href for field-by-field assertion.
func hrefQuery(t *testing.T, href string) url.Values {
	t.Helper()
	i := strings.IndexByte(href, '?')
	if i < 0 {
		return url.Values{}
	}
	v, err := url.ParseQuery(href[i+1:])
	if err != nil {
		t.Fatalf("parse href %q: %v", href, err)
	}
	return v
}

// TestGradeBarsHrefs pins the Grades panel drill links: each letter bar carries its
// grade, the unscored bar carries the sentinel (not a blank), the current window rides
// through as ?range=, and every non-empty bar carries empty=1 so the drilled feed
// matches the panel scope (which counts zero-message sessions). A zero-count bar does
// not link, so it never opens an empty list.
func TestGradeBarsHrefs(t *testing.T) {
	counts := []store.LabeledCount{
		{Key: "A", Count: 5},
		{Key: "D", Count: 0}, // zero-count: no link
		{Key: "", Count: 4},  // the unscored bucket
	}
	rows := GradeBars(counts, store.SessionFilter{}, "30d")

	byLabel := map[string]DistRow{}
	for _, r := range rows {
		byLabel[r.Label] = r
	}

	a := byLabel["A"]
	if a.Href == "" {
		t.Fatal("the A bar should link")
	}
	q := hrefQuery(t, a.Href)
	if q.Get("grade") != "A" || q.Get("range") != "30d" || q.Get("empty") != "1" {
		t.Errorf("A drill = %v, want grade=A range=30d empty=1", q)
	}

	if byLabel["D"].Href != "" {
		t.Errorf("a zero-count bar should not link, got %q", byLabel["D"].Href)
	}

	un := byLabel["Unscored"]
	if un.Href == "" {
		t.Fatal("the unscored bar should link")
	}
	uq := hrefQuery(t, un.Href)
	if uq.Get("grade") != UnscoredKey || uq.Get("empty") != "1" {
		t.Errorf("unscored drill = %v, want grade=%s empty=1", uq, UnscoredKey)
	}
}

// TestOutcomeBarsHrefs pins the Outcomes drill links: the outcome, the range, and
// empty=1 all ride through, and a zero-count bar does not link.
func TestOutcomeBarsHrefs(t *testing.T) {
	counts := []store.LabeledCount{
		{Key: "completed", Count: 8},
		{Key: "errored", Count: 0}, // zero-count: no link
	}
	rows := OutcomeBars(counts, store.SessionFilter{}, "7d")

	if rows[0].Href == "" {
		t.Fatal("the completed bar should link")
	}
	q := hrefQuery(t, rows[0].Href)
	if q.Get("outcome") != "completed" || q.Get("range") != "7d" || q.Get("empty") != "1" {
		t.Errorf("completed drill = %v, want outcome=completed range=7d empty=1", q)
	}
	if rows[1].Href != "" {
		t.Errorf("a zero-count outcome bar should not link, got %q", rows[1].Href)
	}
}

// TestBarsHrefDropsAllRange pins that the "all" window (and the empty key) is dropped
// from a drill link rather than carried as a chip that would window nothing: the bare,
// unwindowed feed is what "all" means. empty=1 still rides through.
func TestBarsHrefDropsAllRange(t *testing.T) {
	for _, rng := range []string{"", "all"} {
		rows := GradeBars([]store.LabeledCount{{Key: "A", Count: 1}}, store.SessionFilter{}, rng)
		q := hrefQuery(t, rows[0].Href)
		if q.Get("range") != "" {
			t.Errorf("range=%q should drop the window from the drill, got range=%q", rng, q.Get("range"))
		}
		if q.Get("empty") != "1" {
			t.Errorf("range=%q drill should still carry empty=1, got %v", rng, q)
		}
	}
}

// TestIsGrade pins the session list's grade whitelist: the five letters and the unscored
// sentinel are accepted, and anything else (a lowercase letter, an unknown word, or the
// empty string) is rejected. The accepted set must match what handleSessions validates a
// ?grade= query param against (internal/server/httpapi/web.go), since the handler and this
// whitelist gate the same value.
func TestIsGrade(t *testing.T) {
	for _, v := range []string{"A", "B", "C", "D", "F", UnscoredKey} {
		if !IsGrade(v) {
			t.Errorf("IsGrade(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "a", "Z", "unscored ", "unknown", "AB"} {
		if IsGrade(v) {
			t.Errorf("IsGrade(%q) = true, want false", v)
		}
	}
}

// TestIsOutcome pins the session list's outcome whitelist: the three concrete outcomes and
// the unknown catch-all are accepted, and anything else is rejected. The accepted set must
// match what handleSessions validates a ?outcome= query param against
// (internal/server/httpapi/web.go).
func TestIsOutcome(t *testing.T) {
	for _, v := range []string{"completed", "abandoned", "errored", "unknown"} {
		if !IsOutcome(v) {
			t.Errorf("IsOutcome(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "Completed", "pending", "unscored"} {
		if IsOutcome(v) {
			t.Errorf("IsOutcome(%q) = true, want false", v)
		}
	}
}

// TestConcurrencyBusiestHref pins the busiest-user drill: it carries the user, the
// window, empty=1, and spanned=1 so the feed matches the concurrency panel's cohort
// (which counts every message_count but only measured-span sessions).
func TestConcurrencyBusiestHref(t *testing.T) {
	c := store.ConcurrencyStats{BusiestUser: "ada", BusiestUserPeak: 3}
	q := hrefQuery(t, string(ConcurrencyBusiestHref(c, "30d")))
	if q.Get("user") != "ada" || q.Get("range") != "30d" || q.Get("empty") != "1" || q.Get("spanned") != "1" {
		t.Errorf("busiest drill = %v, want user=ada range=30d empty=1 spanned=1", q)
	}
}
