package web

import (
	"testing"

	"github.com/jssblck/akari/internal/server/store"
)

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
		// The OG card path is the same escaped base with the card suffix, so the og:image tag
		// and the route stay one definition and a space or slash in the name stays escaped.
		if got := PublicOverviewOGPath(in); got != want+"/og.png" {
			t.Errorf("PublicOverviewOGPath(%q) = %q, want %q", in, got, want+"/og.png")
		}
	}
}

func TestPublicProjectPath(t *testing.T) {
	// The public project overview is keyed on the numeric project id at /p/<id>.
	if got := PublicProjectPath(68); got != "/p/68" {
		t.Errorf("PublicProjectPath(68) = %q, want /p/68", got)
	}
	// The OG card paths are the public bases with the card suffix, matching the routes and the
	// og:image tags: /p/<id>/og.png for a project and /s/<public_id>/og.png for a session.
	if got := PublicProjectOGPath(68); got != "/p/68/og.png" {
		t.Errorf("PublicProjectOGPath(68) = %q, want /p/68/og.png", got)
	}
	if got := PublicSessionOGPath("abc123"); got != "/s/abc123/og.png" {
		t.Errorf("PublicSessionOGPath(abc123) = %q, want /s/abc123/og.png", got)
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
