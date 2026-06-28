// Package watch keeps session files synced continuously. fsnotify drives prompt,
// debounced uploads of changed files; a periodic poll catches roots the OS
// watcher cannot cover (network filesystems, watch exhaustion); and a slow full
// rescan is the safety net for anything both missed.
package watch

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/jssblck/akari/internal/client/discover"
	"github.com/jssblck/akari/internal/client/syncer"
)

// SyncFunc syncs one file. watch depends on this rather than the concrete syncer
// so it can be tested without a server.
type SyncFunc func(ctx context.Context, f discover.File) syncer.Result

// Options tune the watch timers. Zero values fall back to defaults.
type Options struct {
	Debounce time.Duration // quiet period before uploading a changed file
	Poll     time.Duration // mtime/size diff interval for the polling fallback
	Rescan   time.Duration // full rediscover-and-sync safety net interval
	Logf     func(string, ...any)
}

func (o Options) withDefaults() Options {
	if o.Debounce <= 0 {
		o.Debounce = 500 * time.Millisecond
	}
	if o.Poll <= 0 {
		o.Poll = 3 * time.Second
	}
	if o.Rescan <= 0 {
		o.Rescan = 15 * time.Minute
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return o
}

// Watcher watches a set of roots and syncs changed session files.
type Watcher struct {
	roots []discover.Root
	sync  SyncFunc
	opt   Options
}

// New builds a Watcher.
func New(roots []discover.Root, sync SyncFunc, opt Options) *Watcher {
	return &Watcher{roots: roots, sync: sync, opt: opt.withDefaults()}
}

type fileMeta struct {
	mod  time.Time
	size int64
}

// Run watches until ctx is cancelled, then returns ctx.Err(). It performs an
// initial full sync pass before entering the event loop.
func (w *Watcher) Run(ctx context.Context) error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer fsw.Close()

	watched := map[string]bool{}
	for _, r := range w.roots {
		w.addRecursive(fsw, watched, r.Dir)
	}

	rs := &runState{
		w:     w,
		dirty: map[discover.File]struct{}{},
		wake:  make(chan struct{}, 1),
	}
	done := make(chan struct{})
	go func() {
		rs.worker(ctx)
		close(done)
	}()

	// Initial pass: discover everything, seed the poll baseline, and sync all.
	known := map[string]fileMeta{}
	for _, f := range w.discover() {
		if m, ok := statMeta(f.Path); ok {
			known[f.Path] = m
		}
		rs.mark(f)
	}

	pending := map[discover.File]time.Time{}
	flush := time.NewTicker(flushInterval(w.opt.Debounce))
	poll := time.NewTicker(w.opt.Poll)
	rescan := time.NewTicker(w.opt.Rescan)
	defer flush.Stop()
	defer poll.Stop()
	defer rescan.Stop()

	for {
		select {
		case <-ctx.Done():
			<-done // let the worker finish the file it is on, then exit
			return ctx.Err()

		case ev, ok := <-fsw.Events:
			if !ok {
				continue
			}
			w.handleEvent(fsw, watched, ev, pending)

		case err, ok := <-fsw.Errors:
			if ok && err != nil {
				w.opt.Logf("watch error: %v", err)
			}

		case now := <-flush.C:
			for f, deadline := range pending {
				if !now.Before(deadline) {
					rs.mark(f)
					delete(pending, f)
				}
			}

		case <-poll.C:
			// Fallback: stat every discovered file and queue the changed ones.
			for _, f := range w.discover() {
				m, ok := statMeta(f.Path)
				if !ok {
					continue
				}
				if known[f.Path] != m {
					known[f.Path] = m
					pending[f] = time.Now().Add(w.opt.Debounce)
				}
			}

		case <-rescan.C:
			// Safety net: re-add any new directories and re-sync everything.
			for _, r := range w.roots {
				w.addRecursive(fsw, watched, r.Dir)
			}
			for _, f := range w.discover() {
				if m, ok := statMeta(f.Path); ok {
					known[f.Path] = m
				}
				rs.mark(f)
			}
		}
	}
}

// runState holds the dirty set shared between the event loop (producer) and the
// worker (consumer). The set is unbounded but deduplicated by file, so no change
// is ever dropped and a busy file is collapsed to a single pending sync.
type runState struct {
	w     *Watcher
	mu    sync.Mutex
	dirty map[discover.File]struct{}
	wake  chan struct{}
}

// mark records a file as needing a sync and nudges the worker.
func (r *runState) mark(f discover.File) {
	r.mu.Lock()
	r.dirty[f] = struct{}{}
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default: // a wake is already pending; the worker will drain everything
	}
}

// pop removes and returns one dirty file, or ok=false when the set is empty.
func (r *runState) pop() (discover.File, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for f := range r.dirty {
		delete(r.dirty, f)
		return f, true
	}
	return discover.File{}, false
}

// worker drains the dirty set one file at a time. Uploads are idempotent, so a
// file re-marked while in flight simply syncs again on the next drain.
func (r *runState) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		}
		for {
			if ctx.Err() != nil {
				return
			}
			f, ok := r.pop()
			if !ok {
				break
			}
			res := r.w.sync(ctx, f)
			switch {
			case res.Skipped:
				r.w.opt.Logf("skip %s: %s", f.Path, res.Reason)
			case res.Err != nil:
				r.w.opt.Logf("error %s: %v", f.Path, res.Err)
			case res.UploadedBytes > 0:
				r.w.opt.Logf("uploaded %s -> %s (%d bytes)", f.Path, res.Destination(), res.UploadedBytes)
			}
		}
	}
}

// handleEvent reacts to one filesystem event: new directories are watched
// recursively, and changed session files are scheduled after the debounce.
func (w *Watcher) handleEvent(fsw *fsnotify.Watcher, watched map[string]bool, ev fsnotify.Event, pending map[discover.File]time.Time) {
	if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) == 0 {
		return
	}
	if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
		w.addRecursive(fsw, watched, ev.Name)
		return
	}
	if f, ok := w.fileFor(ev.Name); ok {
		pending[f] = time.Now().Add(w.opt.Debounce)
	}
}

// fileFor classifies an event path: the root whose directory contains it gives
// the agent, and the agent's filename pattern confirms it is a session file.
func (w *Watcher) fileFor(path string) (discover.File, bool) {
	base := filepath.Base(path)
	for _, r := range w.roots {
		if within(r.Dir, path) && discover.Matches(r.Agent, base) {
			return discover.File{Agent: r.Agent, Path: path}, true
		}
	}
	return discover.File{}, false
}

func (w *Watcher) discover() []discover.File {
	files, err := discover.Discover(w.roots)
	if err != nil {
		w.opt.Logf("discover: %v", err)
	}
	return files
}

// addRecursive adds dir and all of its subdirectories to the watcher.
func (w *Watcher) addRecursive(fsw *fsnotify.Watcher, watched map[string]bool, dir string) {
	if dir == "" {
		return
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && !watched[p] {
			if addErr := fsw.Add(p); addErr == nil {
				watched[p] = true
			}
		}
		return nil
	})
}

func statMeta(path string) (fileMeta, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return fileMeta{}, false
	}
	return fileMeta{mod: info.ModTime(), size: info.Size()}, true
}

func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// flushInterval picks how often to check the debounce map: often enough to honor
// the debounce, but not busier than needed.
func flushInterval(debounce time.Duration) time.Duration {
	d := debounce / 2
	if d < 100*time.Millisecond {
		d = 100 * time.Millisecond
	}
	return d
}
