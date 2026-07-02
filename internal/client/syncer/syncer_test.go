package syncer

import (
	"context"
	"testing"

	"github.com/jssblck/akari/internal/client/discover"
	"github.com/jssblck/akari/internal/client/resolve"
	"github.com/jssblck/akari/internal/client/upload"
)

// fakeResolver resolves every file to a fixed result, so a test drives SyncOne
// without a real file or git remote.
type fakeResolver struct{ res resolve.Result }

func (f fakeResolver) Resolve(context.Context, discover.File) resolve.Result { return f.res }

// recordingUploader captures the Target SyncOne hands it, so a test can assert the
// syncer built it from the right inputs, and never touches a server.
type recordingUploader struct{ last upload.Target }

func (u *recordingUploader) SyncFile(_ context.Context, t upload.Target) (upload.Outcome, error) {
	u.last = t
	return upload.Outcome{Action: upload.ActionUpToDate}, nil
}

// TestSyncOneCarriesFinalize proves the finalize argument threaded into New reaches
// the upload.Target SyncOne builds: the downstream half of the `akari sync --finalize`
// wiring. Dropping the constructor argument, or omitting Finalize from the Target,
// would flip the assertion. It checks the other carried fields too, so a reshuffle
// that preserved Finalize but dropped a neighbor still fails.
func TestSyncOneCarriesFinalize(t *testing.T) {
	res := resolve.Result{
		Kind:       resolve.KindRemote,
		ProjectKey: "github.com/o/r",
		Header:     resolve.Header{SourceID: "s1", Cwd: "/x", GitBranch: "main"},
	}
	for _, finalize := range []bool{true, false} {
		up := &recordingUploader{}
		s := New(fakeResolver{res: res}, up, "m", finalize)
		if r := s.SyncOne(context.Background(), discover.File{Agent: "codex"}); r.Err != nil {
			t.Fatalf("finalize=%v: SyncOne returned error: %v", finalize, r.Err)
		}
		if up.last.Finalize != finalize {
			t.Errorf("finalize=%v: Target.Finalize = %v, want %v", finalize, up.last.Finalize, finalize)
		}
		if up.last.Agent != "codex" || up.last.Machine != "m" || up.last.SourceID != "s1" || up.last.ProjectKey != "github.com/o/r" {
			t.Errorf("finalize=%v: Target = %+v, want agent/machine/source/project carried through", finalize, up.last)
		}
	}
}

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
