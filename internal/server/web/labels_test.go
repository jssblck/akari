package web

import (
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

// A remote project is headed by its canonical remote key; a local (standalone or
// orphaned) project by its folder name, keeping the synthetic "local:machine:path"
// key out of every heading.
func TestProjectTitle(t *testing.T) {
	remote := store.ProjectSummary{Kind: "remote", RemoteKey: "github.com/jssblck/akari", DisplayName: "akari"}
	if got := ProjectTitle(remote); got != "github.com/jssblck/akari" {
		t.Errorf("remote title = %q", got)
	}
	for _, kind := range []string{"standalone", "orphaned"} {
		local := store.ProjectSummary{Kind: kind, RemoteKey: "local:rig:/home/grace/scratch", DisplayName: "scratch"}
		if got := ProjectTitle(local); got != "scratch" {
			t.Errorf("%s title = %q, want folder name", kind, got)
		}
	}
}

// SessionProjectLabel and ProjectLabel apply the same rule from a SessionDetail
// and from bare fields (the OG card path), so the page heading and the share card
// can never disagree.
func TestSessionProjectLabel(t *testing.T) {
	remote := store.SessionDetail{ProjectKind: "remote", ProjectName: "akari", ProjectKey: "github.com/jssblck/akari"}
	if got := SessionProjectLabel(remote); got != "github.com/jssblck/akari" {
		t.Errorf("remote label = %q", got)
	}
	local := store.SessionDetail{ProjectKind: "standalone", ProjectName: "scratch", ProjectKey: "local:rig:/x/scratch"}
	if got := SessionProjectLabel(local); got != "scratch" {
		t.Errorf("local label = %q", got)
	}
	if got := ProjectLabel("orphaned", "scratch", "local:rig:/x/scratch"); got != "scratch" {
		t.Errorf("ProjectLabel local = %q", got)
	}
	if got := ProjectLabel("remote", "akari", "github.com/jssblck/akari"); got != "github.com/jssblck/akari" {
		t.Errorf("ProjectLabel remote = %q", got)
	}
}

// SessionPageTitle prefers the session's own summary line and falls back to a
// stable "<project> session" label, so a shared link and the in-app tab read the
// same either way.
func TestSessionPageTitle(t *testing.T) {
	titled := store.SessionDetail{
		SessionSummary: store.SessionSummary{Title: "Fix the stale cache read"},
		ProjectKind:    "remote", ProjectKey: "github.com/jssblck/akari",
	}
	if got := SessionPageTitle(titled); got != "Fix the stale cache read" {
		t.Errorf("titled = %q", got)
	}
	untitled := store.SessionDetail{ProjectKind: "remote", ProjectKey: "github.com/jssblck/akari"}
	if got := SessionPageTitle(untitled); got != "github.com/jssblck/akari session" {
		t.Errorf("untitled = %q", got)
	}
}
