package web

import (
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

func TestSessionsPath(t *testing.T) {
	cases := []struct {
		name string
		f    store.SessionFilter
		want string
	}{
		{"empty", store.SessionFilter{}, "/sessions"},
		{"agent", store.SessionFilter{Agent: "claude"}, "/sessions?agent=claude"},
		{"project", store.SessionFilter{ProjectID: 7}, "/sessions?project=7"},
		{"multi", store.SessionFilter{Agent: "claude", Username: "jess", ProjectID: 7}, "/sessions?agent=claude&project=7&user=jess"},
	}
	for _, c := range cases {
		if got := SessionsPath(c.f); got != c.want {
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
		// The OG card path is the same escaped base with the card suffix, so the og:image tag
		// and the route stay one definition and a space or slash in the name stays escaped.
		if got := PublicOverviewOGPath(in); got != want+"/og.png" {
			t.Errorf("PublicOverviewOGPath(%q) = %q, want %q", in, got, want+"/og.png")
		}
	}
}

func TestPublicProjectPath(t *testing.T) {
	// The public project overview is keyed on the numeric project id at /p/<id>, and the
	// href form is the same path wrapped as a sanitized SafeURL.
	if got := PublicProjectPath(68); got != "/p/68" {
		t.Errorf("PublicProjectPath(68) = %q, want /p/68", got)
	}
	if got := string(PublicProjectHref(68)); got != "/p/68" {
		t.Errorf("PublicProjectHref(68) = %q, want /p/68", got)
	}
	// The OG card paths are the public bases with the card suffix, matching the routes and the
	// og:image tags: /p/<id>/og.png for a project and /s/<public_id>/og.png for a session.
	if got := PublicProjectOGPath(68); got != "/p/68/og.png" {
		t.Errorf("PublicProjectOGPath(68) = %q, want /p/68/og.png", got)
	}
	if got := PublicSessionOGPath("abc123"); got != "/s/abc123/og.png" {
		t.Errorf("PublicSessionOGPath(abc123) = %q, want /s/abc123/og.png", got)
	}
	// The POST targets for the publicity control match the routes registered in server.go.
	if got := ProjectPublishPath(68); got != "/projects/68/overview/publish" {
		t.Errorf("ProjectPublishPath(68) = %q", got)
	}
	if got := ProjectUnpublishPath(68); got != "/projects/68/overview/unpublish" {
		t.Errorf("ProjectUnpublishPath(68) = %q", got)
	}
}

// TestPlainBarsDropDrillLinks pins the public project quality band's contract: the
// Plain bar builders carry the same labels, counts, colors, and widths as the linked
// GradeBars/OutcomeBars, but leave Href empty so distributionPanel renders bars rather
// than links into the private session feed a logged-out viewer cannot open.
func TestPlainBarsDropDrillLinks(t *testing.T) {
	grades := []store.LabeledCount{{Key: "A", Count: 3}, {Key: "F", Count: 1}, {Key: "", Count: 2}}
	linked := GradeBars(grades, store.SessionFilter{ProjectID: 7}, "30d")
	plain := GradeBarsPlain(grades)
	if len(plain) != len(linked) {
		t.Fatalf("GradeBarsPlain len = %d, want %d", len(plain), len(linked))
	}
	for i := range plain {
		if plain[i].Label != linked[i].Label || plain[i].Count != linked[i].Count ||
			plain[i].Color != linked[i].Color || plain[i].Pct != linked[i].Pct {
			t.Errorf("GradeBarsPlain[%d] = %+v, want same label/count/color/pct as %+v", i, plain[i], linked[i])
		}
		if plain[i].Href != "" {
			t.Errorf("GradeBarsPlain[%d] Href = %q, want empty (no drill-through)", i, plain[i].Href)
		}
	}
	// A non-empty grade bucket links in the drilling variant, confirming the plain
	// variant genuinely dropped a link rather than there being none to drop.
	if linked[0].Href == "" {
		t.Fatal("GradeBars should link a non-empty bucket; the plain test would be vacuous otherwise")
	}

	outcomes := []store.LabeledCount{{Key: "completed", Count: 4}, {Key: "abandoned", Count: 1}}
	linkedOut := OutcomeBars(outcomes, store.SessionFilter{ProjectID: 7}, "30d")
	plainOut := OutcomeBarsPlain(outcomes)
	if len(plainOut) != len(linkedOut) {
		t.Fatalf("OutcomeBarsPlain len = %d, want %d", len(plainOut), len(linkedOut))
	}
	for i := range plainOut {
		if plainOut[i].Label != linkedOut[i].Label || plainOut[i].Count != linkedOut[i].Count ||
			plainOut[i].Color != linkedOut[i].Color || plainOut[i].Pct != linkedOut[i].Pct {
			t.Errorf("OutcomeBarsPlain[%d] = %+v, want same label/count/color/pct as %+v", i, plainOut[i], linkedOut[i])
		}
		if plainOut[i].Href != "" {
			t.Errorf("OutcomeBarsPlain[%d] Href = %q, want empty", i, plainOut[i].Href)
		}
	}
	if linkedOut[0].Href == "" {
		t.Fatal("OutcomeBars should link a non-empty bucket; the plain test would be vacuous otherwise")
	}
}

func TestFacetToggleHrefs(t *testing.T) {
	// Selecting a facet from empty sets it.
	if got := string(AgentFacetHref(store.SessionFilter{}, "claude")); got != "/sessions?agent=claude" {
		t.Errorf("select agent = %q", got)
	}
	// Toggling the active value clears it.
	if got := string(AgentFacetHref(store.SessionFilter{Agent: "claude"}, "claude")); got != "/sessions" {
		t.Errorf("clear agent = %q", got)
	}
	// Toggling a facet preserves the rest of the selection.
	if got := string(AgentFacetHref(store.SessionFilter{Username: "jess"}, "claude")); got != "/sessions?agent=claude&user=jess" {
		t.Errorf("preserve other = %q", got)
	}
	// Project toggles by id and preserves other fields.
	if got := string(ProjectFacetHref(store.SessionFilter{Agent: "codex"}, 3)); got != "/sessions?agent=codex&project=3" {
		t.Errorf("select project = %q", got)
	}
	if got := string(ProjectFacetHref(store.SessionFilter{Agent: "codex", ProjectID: 3}, 3)); got != "/sessions?agent=codex" {
		t.Errorf("clear project = %q", got)
	}
}

func TestFacetHrefPreservesSort(t *testing.T) {
	// A facet toggle holds the active sort so filtering does not silently reset the
	// reader's chosen order.
	f := store.SessionFilter{Sort: "tokens", Desc: true}
	if got := string(AgentFacetHref(f, "claude")); got != "/sessions?agent=claude&dir=desc&sort=tokens" {
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

func TestOverviewPath(t *testing.T) {
	cases := []struct {
		name string
		rng  string
		ids  []int64
		want string
	}{
		{"range only", "30d", nil, "/overview?range=30d"},
		{"range and users", "7d", []int64{2, 5}, "/overview?range=7d&user=2&user=5"},
		{"users no range", "", []int64{9}, "/overview?user=9"},
		{"nothing", "", nil, "/overview"},
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
