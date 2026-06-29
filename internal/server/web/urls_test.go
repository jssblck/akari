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
