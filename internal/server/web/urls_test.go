package web

import (
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

func TestSessionsPath(t *testing.T) {
	cases := []struct {
		name string
		f    store.SessionFilter
		rng  string
		want string
	}{
		{"empty", store.SessionFilter{}, "", "/sessions"},
		{"agent", store.SessionFilter{Agent: "claude"}, "", "/sessions?agent=claude"},
		{"project", store.SessionFilter{ProjectID: 7}, "", "/sessions?project=7"},
		{"multi", store.SessionFilter{Agent: "claude", Username: "jess", ProjectID: 7}, "", "/sessions?agent=claude&project=7&user=jess"},
		// A bounded range key rides the query so a drill-down opens the same window it counted.
		{"bounded range", store.SessionFilter{Agent: "claude"}, "30d", "/sessions?agent=claude&range=30d"},
		// The all-history window, the empty key, and any unknown value add no bound: the feed's
		// natural form is unbounded, so the bare path (plus other filters) is what round-trips.
		{"all range unbounded", store.SessionFilter{Agent: "claude"}, "all", "/sessions?agent=claude"},
		{"unknown range unbounded", store.SessionFilter{Agent: "claude"}, "bogus", "/sessions?agent=claude"},
		{"range only", store.SessionFilter{}, "7d", "/sessions?range=7d"},
	}
	for _, c := range cases {
		if got := SessionsPath(c.f, c.rng); got != c.want {
			t.Errorf("SessionsPath(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPublicOverviewPath(t *testing.T) {
	cases := map[string]string{
		"grace":        "/u/grace",
		"ada lovelace": "/u/ada%20lovelace", // a space is escaped, never a raw segment break
		"a/b":          "/u/a%2Fb",          // a slash cannot escape the /u/ segment
	}
	for in, want := range cases {
		if got := PublicOverviewPath(in); got != want {
			t.Errorf("PublicOverviewPath(%q) = %q, want %q", in, got, want)
		}
		// The href form is the same path, wrapped as a sanitized SafeURL.
		if got := string(PublicOverviewHref(in)); got != want {
			t.Errorf("PublicOverviewHref(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFacetToggleHrefs(t *testing.T) {
	// Selecting a facet from empty sets it.
	if got := string(AgentFacetHref(store.SessionFilter{}, "claude", "")); got != "/sessions?agent=claude" {
		t.Errorf("select agent = %q", got)
	}
	// Toggling the active value clears it.
	if got := string(AgentFacetHref(store.SessionFilter{Agent: "claude"}, "claude", "")); got != "/sessions" {
		t.Errorf("clear agent = %q", got)
	}
	// Toggling a facet preserves the rest of the selection.
	if got := string(AgentFacetHref(store.SessionFilter{Username: "jess"}, "claude", "")); got != "/sessions?agent=claude&user=jess" {
		t.Errorf("preserve other = %q", got)
	}
	// Project toggles by id and preserves other fields.
	if got := string(ProjectFacetHref(store.SessionFilter{Agent: "codex"}, 3, "")); got != "/sessions?agent=codex&project=3" {
		t.Errorf("select project = %q", got)
	}
	if got := string(ProjectFacetHref(store.SessionFilter{Agent: "codex", ProjectID: 3}, 3, "")); got != "/sessions?agent=codex" {
		t.Errorf("clear project = %q", got)
	}
	// A facet toggle holds the active bounded window so clearing one filter keeps the scope.
	if got := string(AgentFacetHref(store.SessionFilter{Username: "jess"}, "claude", "30d")); got != "/sessions?agent=claude&range=30d&user=jess" {
		t.Errorf("facet toggle should preserve range, got %q", got)
	}
}

func TestFacetHrefPreservesSort(t *testing.T) {
	// A facet toggle holds the active sort so filtering does not silently reset the
	// reader's chosen order.
	f := store.SessionFilter{Sort: "tokens", Desc: true}
	if got := string(AgentFacetHref(f, "claude", "")); got != "/sessions?agent=claude&dir=desc&sort=tokens" {
		t.Errorf("facet toggle should preserve sort, got %q", got)
	}
}

func TestAnyFilterActive(t *testing.T) {
	if AnyFilterActive(store.SessionFilter{}) {
		t.Error("empty filter should be inactive")
	}
	for _, f := range []store.SessionFilter{
		{Agent: "claude"}, {Username: "jess"}, {Machine: "box"}, {ProjectID: 1},
	} {
		if !AnyFilterActive(f) {
			t.Errorf("filter %+v should be active", f)
		}
	}
}

// TestRangeBounds pins the sessions-feed range whitelist: only a known bounded key (a positive
// day span) bounds the feed; "all", the empty string, and any unknown value read as unbounded so
// the bare feed stays all-history.
func TestRangeBounds(t *testing.T) {
	for _, k := range []string{"7d", "30d", "90d", "year"} {
		if !RangeBounds(k) {
			t.Errorf("RangeBounds(%q) = false, want true (bounded window)", k)
		}
	}
	for _, k := range []string{"all", "", "bogus", "month"} {
		if RangeBounds(k) {
			t.Errorf("RangeBounds(%q) = true, want false (unbounded)", k)
		}
	}
}

func TestOverviewPath(t *testing.T) {
	cases := []struct {
		name string
		rng  string
		ids  []int64
		want string
	}{
		{"range only", "30d", nil, "/?range=30d"},
		{"range and users", "7d", []int64{2, 5}, "/?range=7d&user=2&user=5"},
		{"users no range", "", []int64{9}, "/?user=9"},
		{"nothing", "", nil, "/"},
	}
	for _, c := range cases {
		if got := OverviewPath(c.rng, c.ids); got != c.want {
			t.Errorf("OverviewPath(%s) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSelectedUserIDs(t *testing.T) {
	users := []store.User{{ID: 2, Username: "ada"}, {ID: 5, Username: "grace"}, {ID: 9, Username: "hopper"}}
	// Valid ids resolve and come back in users-list order, not query order.
	if got := SelectedUserIDs([]string{"9", "2"}, users); len(got) != 2 || got[0] != 2 || got[1] != 9 {
		t.Errorf("SelectedUserIDs should keep known ids in users order, got %v", got)
	}
	// Unknown and non-numeric ids drop out silently.
	if got := SelectedUserIDs([]string{"5", "999", "abc"}, users); len(got) != 1 || got[0] != 5 {
		t.Errorf("SelectedUserIDs should drop unknown/non-numeric ids, got %v", got)
	}
	// No selection is nil (the unscoped "all users" view).
	if got := SelectedUserIDs(nil, users); got != nil {
		t.Errorf("empty selection should be nil, got %v", got)
	}
	if got := SelectedUserIDs([]string{"oops"}, users); got != nil {
		t.Errorf("all-invalid selection should be nil, got %v", got)
	}
}

func TestNavClass(t *testing.T) {
	if got := navClass("sessions", "sessions"); got != "nav active" {
		t.Errorf("active navClass = %q", got)
	}
	if got := navClass("sessions", "overview"); got != "nav" {
		t.Errorf("inactive navClass = %q", got)
	}
}

func TestSessionRowProject(t *testing.T) {
	remote := store.SessionRow{ProjectKey: "github.com/jssblck/akari", ProjectName: "akari", ProjectKind: "remote"}
	if got := SessionRowProject(remote); got != "github.com/jssblck/akari" {
		t.Errorf("remote label = %q", got)
	}
	local := store.SessionRow{ProjectKey: "local:rig:/home/sam/scratch", ProjectName: "scratch", ProjectKind: "standalone"}
	if got := SessionRowProject(local); got != "scratch" {
		t.Errorf("local label = %q", got)
	}
}

func TestProjectFacetLabel(t *testing.T) {
	remote := store.ProjectFacet{Key: "github.com/jssblck/akari", Name: "akari", Kind: "remote"}
	if got := ProjectFacetLabel(remote); got != "github.com/jssblck/akari" {
		t.Errorf("remote label = %q", got)
	}
	local := store.ProjectFacet{Key: "local:rig:/x/scratch", Name: "scratch", Kind: "orphaned"}
	if got := ProjectFacetLabel(local); got != "scratch" {
		t.Errorf("local label = %q", got)
	}
}
