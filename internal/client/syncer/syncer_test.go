package syncer

import (
	"testing"

	"github.com/jssblck/akari/internal/client/resolve"
)

// TestDestination covers the label precedence: a remote session shows its
// project key, a standalone session backed by a live worktree shows the shared
// repo root it grouped under, any other local session falls back to its working
// directory, and a session with no location at all shows only its kind.
func TestDestination(t *testing.T) {
	cases := []struct {
		name string
		r    Result
		want string
	}{
		{
			name: "remote prefers project key",
			r:    Result{Kind: resolve.KindRemote, ProjectKey: "github.com/o/r", Cwd: "/x"},
			want: "github.com/o/r",
		},
		{
			name: "standalone prefers local root over cwd",
			r:    Result{Kind: resolve.KindStandalone, LocalRoot: "/home/grace/repo", Cwd: "/home/grace/wt/a"},
			want: "standalone:/home/grace/repo",
		},
		{
			name: "standalone falls back to cwd",
			r:    Result{Kind: resolve.KindStandalone, Cwd: "/home/grace/wt/a"},
			want: "standalone:/home/grace/wt/a",
		},
		{
			name: "orphaned with no location shows only the kind",
			r:    Result{Kind: resolve.KindOrphaned},
			want: "orphaned",
		},
	}
	for _, c := range cases {
		if got := c.r.Destination(); got != c.want {
			t.Errorf("%s: Destination() = %q, want %q", c.name, got, c.want)
		}
	}
}
