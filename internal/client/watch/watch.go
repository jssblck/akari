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
	Poll     time.Duration // mtime/size re-stat interval for the polling fallback
	Discover time.Duration // interval to re-walk the roots for newly created files
	Rescan   time.Duration // full rediscover-and-sync safety net interval
	// Excludes are glob patterns of paths to skip (see discover.Excluder). They
	// keep an ignored location out of discovery, the poll, and event handling.
	Excludes []string
	Logf     func(string, ...any)
}

func (o Options) withDefaults() Options {
	if o.Debounce <= 0 {
		o.Debounce = 500 * time.Millisecond
	}
	if o.Poll <= 0 {
		o.Poll = 3 * time.Second
	}
	if o.Discover <= 0 {
		o.Discover = 30 * time.Second
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
	ex    discover.Excluder
}

// New builds a Watcher.
func New(roots []discover.Root, sync SyncFunc, opt Options) *Watcher {
	o := opt.withDefaults()
	return &Watcher{roots: roots, sync: sync, opt: o, ex: discover.NewExcluder(o.Excludes)}
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

	// Initial pass: discover everything, seed the poll baseline, and sync all. The
	// baseline is keyed by File so the poll can re-stat the known set directly and
	// has the File in hand to queue a changed one, without re-walking the tree.
	known := map[discover.File]fileMeta{}
	for _, f := range w.discover() {
		if m, ok := statMeta(f.Path); ok {
			known[f] = m
		}
		rs.mark(f)
	}

	pending := map[discover.File]time.Time{}
	flush := time.NewTicker(flushInterval(w.opt.Debounce))
	poll := time.NewTicker(w.opt.Poll)
	disco := time.NewTicker(w.opt.Discover)
	rescan := time.NewTicker(w.opt.Rescan)
	defer flush.Stop()
	defer poll.Stop()
	defer disco.Stop()
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
			w.handleEvent(fsw, watched, ev, known, pending)

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
			// Fallback for changes the OS watcher missed: re-stat only the files we
			// already know about (no tree walk) and queue the changed ones. Finding
			// newly created files is the discover ticker's job below, so the frequent
			// poll stays O(known files) of stat syscalls rather than re-walking and
			// re-sorting the whole session tree every few seconds.
			for f, prev := range known {
				m, ok := statMeta(f.Path)
				if !ok {
					delete(known, f) // gone from disk; stop tracking it
					continue
				}
				if m != prev {
					known[f] = m
					pending[f] = time.Now().Add(w.opt.Debounce)
				}
			}

		case <-disco.C:
			// Catch files created on a root the OS watcher cannot cover (a network
			// filesystem, or one past the watch limit), where no Create event fires.
			// This walks the tree, but on a slower cadence than the poll, so a brand
			// new file there appears within this interval rather than every poll
			// paying for a walk. A file fsnotify did see is already syncing via its
			// Create event; this only adds the ones missing from the baseline.
			for _, f := range w.discover() {
				if _, ok := known[f]; ok {
					continue
				}
				if m, ok := statMeta(f.Path); ok {
					known[f] = m
				}
				rs.mark(f)
			}

		case <-rescan.C:
			// Safety net: re-add any new directories and re-sync everything.
			for _, r := range w.roots {
				w.addRecursive(fsw, watched, r.Dir)
			}
			for _, f := range w.discover() {
				if m, ok := statMeta(f.Path); ok {
					known[f] = m
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
//
// Syncs run on a context detached from ctx so the file the worker is on finishes
// uploading after a Ctrl-C; once that file is done the worker stops instead of
// draining the rest of the backlog. A second Ctrl-C exits the process outright
// (handled by the signal layer), so a slow upload never blocks shutdown forever.
func (r *runState) worker(ctx context.Context) {
	work := context.WithoutCancel(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		}
		for {
			f, ok := r.pop()
			if !ok {
				break
			}
			res := r.w.sync(work, f)
			switch {
			case res.Skipped:
				r.w.opt.Logf("skip %s: %s", f.Path, res.Reason)
			case res.Err != nil:
				r.w.opt.Logf("error %s: %v", f.Path, res.Err)
			case res.UploadedBytes > 0:
				r.w.opt.Logf("uploaded %s -> %s (%d bytes)", f.Path, res.Destination(), res.UploadedBytes)
			}
			if ctx.Err() != nil {
				return // finished the current file; stop without draining the backlog
			}
		}
	}
}

// handleEvent reacts to one filesystem event: new directories are watched
// recursively, and changed session files are scheduled after the debounce. An
// accepted file also enters the poll's known set, so the fast poll covers a Write
// the OS watcher may later miss on that file rather than leaving it uncovered until
// the slower discover ticker folds it in.
func (w *Watcher) handleEvent(fsw *fsnotify.Watcher, watched map[string]bool, ev fsnotify.Event, known map[discover.File]fileMeta, pending map[discover.File]time.Time) {
	if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Rename) == 0 {
		return
	}
	if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
		w.addRecursive(fsw, watched, ev.Name)
		return
	}
	if f, ok := w.fileFor(ev.Name); ok {
		pending[f] = time.Now().Add(w.opt.Debounce)
		if _, tracked := known[f]; !tracked {
			if m, ok := statMeta(f.Path); ok {
				known[f] = m
			}
		}
	}
}

// fileFor classifies an event path: the root whose directory contains it gives
// the agent, and the agent's filename pattern confirms it is a session file.
func (w *Watcher) fileFor(path string) (discover.File, bool) {
	if w.ex.Excluded(path) {
		return discover.File{}, false
	}
	base := filepath.Base(path)
	for _, r := range w.roots {
		if within(r.Dir, path) && discover.Matches(r.Agent, base) {
			return discover.File{Agent: r.Agent, Root: r.Dir, Path: path}, true
		}
	}
	return discover.File{}, false
}

func (w *Watcher) discover() []discover.File {
	files, err := discover.Discover(w.roots, w.ex)
	if err != nil {
		w.opt.Logf("discover: %v", err)
	}
	return files
}

// addRecursive adds dir and all of its subdirectories to the watcher, skipping any
// excluded subtree so the watch never spends an fsnotify slot on a directory whose
// files would be filtered out anyway.
func (w *Watcher) addRecursive(fsw *fsnotify.Watcher, watched map[string]bool, dir string) {
	if dir == "" {
		return
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if p != dir && w.ex.ExcludedDir(p) {
			return filepath.SkipDir
		}
		if !watched[p] {
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
