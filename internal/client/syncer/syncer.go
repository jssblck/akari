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
// successful upload action.
type Result struct {
	File          discover.File
	ProjectKey    string
	Skipped       bool
	Reason        string
	Err           error
	Action        upload.Action
	UploadedBytes int64
	MessageCount  int
}

// Syncer resolves files and uploads them to one server as one machine.
type Syncer struct {
	resolver *resolve.Resolver
	uploader *upload.Client
	machine  string
}

// New builds a Syncer.
func New(r *resolve.Resolver, u *upload.Client, machine string) *Syncer {
	return &Syncer{resolver: r, uploader: u, machine: machine}
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
		ProjectKey: res.ProjectKey,
		GitBranch:  res.Header.GitBranch,
		Cwd:        res.Header.Cwd,
		Machine:    s.machine,
	})
	if err != nil {
		return Result{File: f, ProjectKey: res.ProjectKey, Err: err}
	}
	return Result{
		File:          f,
		ProjectKey:    res.ProjectKey,
		Action:        out.Action,
		UploadedBytes: out.UploadedBytes,
		MessageCount:  out.MessageCount,
	}
}
