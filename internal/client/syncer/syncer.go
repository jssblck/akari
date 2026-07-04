// Package syncer combines resolution and upload for a single session file. Both
// the one-shot sync command and the watch loop drive files through it, so the
// "resolve to a project, then push the gap" logic lives in one place.
package syncer

import (
	"context"

	"github.com/jssblck/akari/internal/client/discover"
	"github.com/jssblck/akari/internal/client/resolve"
	"github.com/jssblck/akari/internal/client/upload"
)

// Result is the outcome of syncing one file: a skip with a reason, an error, or a
// successful upload action. Kind classifies how the session resolved; Reason
// carries the detail behind a standalone or orphaned classification (or a skip).
type Result struct {
	File          discover.File
	Kind          resolve.Kind
	ProjectKey    string
	LocalRoot     string
	Cwd           string
	Skipped       bool
	Reason        string
	Err           error
	Action        upload.Action
	UploadedBytes int64
}

// Destination is a short label for where the file was backed up, for logs and
// summaries. A remote session shows its project key; a standalone session backed
// by a live worktree shows the repo root it grouped under; any other local
// session shows its kind and working directory, since it has no remote.
func (r Result) Destination() string {
	if r.ProjectKey != "" {
		return r.ProjectKey
	}
	loc := r.LocalRoot
	if loc == "" {
		loc = r.Cwd
	}
	if loc != "" {
		return string(r.Kind) + ":" + loc
	}
	return string(r.Kind)
}

// fileResolver resolves a discovered session file to its project. *resolve.Resolver
// implements it; the interface is the seam a test substitutes a fake through.
type fileResolver interface {
	Resolve(ctx context.Context, f discover.File) resolve.Result
}

// fileUploader pushes a resolved session's new bytes to the server. *upload.Client
// implements it; the interface lets a test observe the Target the syncer builds
// (in particular that Finalize is carried through) without a live server.
type fileUploader interface {
	SyncFile(ctx context.Context, t upload.Target) (upload.Outcome, error)
}

// Syncer resolves files and uploads them to one server as one machine. When
// finalize is set, every file it syncs is uploaded as terminal: a Codex session's
// withheld trailing turn is flushed now rather than after the idle settle window,
// for ephemeral hosts (CI, cloud sandboxes) whose window never elapses.
type Syncer struct {
	resolver fileResolver
	uploader fileUploader
	machine  string
	finalize bool
}

// New builds a Syncer. finalize forces every synced session's trailing turn to be
// treated as settled (see Syncer); the watch loop and devseed pass false, only
// `akari sync --finalize` passes true.
func New(r fileResolver, u fileUploader, machine string, finalize bool) *Syncer {
	return &Syncer{resolver: r, uploader: u, machine: machine, finalize: finalize}
}

// SyncOne resolves a file to its project and uploads any new bytes. It never
// returns an error directly; failures are reported in Result.Err so a caller
// processing many files can record and continue.
func (s *Syncer) SyncOne(ctx context.Context, f discover.File) Result {
	res := s.resolver.Resolve(ctx, f)
	if res.Skipped {
		return Result{File: f, Skipped: true, Reason: res.Reason}
	}

	out, err := s.uploader.SyncFile(ctx, upload.Target{
		Agent:      f.Agent,
		Path:       f.Path,
		SourceID:   res.Header.SourceID,
		Kind:       string(res.Kind),
		ProjectKey: res.ProjectKey,
		LocalRoot:  res.LocalRoot,
		GitBranch:  res.Header.GitBranch,
		Cwd:        res.Header.Cwd,
		Machine:    s.machine,
		Finalize:   s.finalize,
	})
	if err != nil {
		return Result{File: f, Kind: res.Kind, ProjectKey: res.ProjectKey, LocalRoot: res.LocalRoot, Cwd: res.Header.Cwd, Reason: res.Reason, Err: err}
	}
	return Result{
		File:          f,
		Kind:          res.Kind,
		ProjectKey:    res.ProjectKey,
		LocalRoot:     res.LocalRoot,
		Cwd:           res.Header.Cwd,
		Reason:        res.Reason,
		Action:        out.Action,
		UploadedBytes: out.UploadedBytes,
	}
}
