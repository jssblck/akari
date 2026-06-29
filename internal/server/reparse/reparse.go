// Package reparse is the self-healing reparse backbone: one service that rebuilds
// the parsed projection of stored sessions from their raw bytes, driven from three
// places that must never diverge: the server's startup epoch check, the admin
// Reparse button, and the akari-server reparse CLI. Centralizing the loop here (it
// used to be inline in the CLI) means all three share the same in-process guard,
// the same cross-process advisory lock, the same progress reporting, and the same
// post-reparse blob sweep.
//
// A reparse rebuilds in place: per session it deletes the old projection then
// replays the raw through the reducer, so mid-reparse a session genuinely has
// old-or-absent parsed data. That is why the HTTP layer gates the parsed UI while a
// reparse runs rather than serving half-rebuilt rows. A future improvement could
// build a shadow projection and swap it atomically per session, removing the need
// to gate; this service deliberately does the simpler in-place rebuild and leaves
// gating to the caller.
package reparse

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/store"
)

// defaultReparsePageSize bounds how many reparse targets are resident at once. The
// loop pages through sessions by id, so peak memory is one page plus the session
// being reparsed, not the whole corpus. It seeds each Service's pageSize, which
// tests shrink per-instance to exercise the multi-page path without racing on
// shared state.
const defaultReparsePageSize = 512

// reparseUnlockTimeout caps the advisory-lock release so a stuck unlock cannot
// block shutdown.
const reparseUnlockTimeout = 5 * time.Second

// defaultFleetCacheTTL is how long the cross-process "is a reparse running" answer
// is cached, so gating a parsed-page request does not query pg_locks every time. It
// seeds each Service's fleetTTL, which tests set to zero per-instance to observe
// lock transitions without waiting out the cache or racing on shared state.
const defaultFleetCacheTTL = 2 * time.Second

// Status is a snapshot of the current (or last) reparse. It is what the HTTP
// status endpoint and the SSE progress stream serialize, so its JSON tags are the
// wire contract the frontend reads.
type Status struct {
	// InProgress is true from the moment a reparse is accepted until it finishes,
	// including the brief window before the advisory lock is resolved. The parsed UI
	// gates on it.
	InProgress bool `json:"in_progress"`
	// Done is the number of sessions processed so far (successes plus failures),
	// Total the number to process, Failed the subset that errored. Done/Total drive
	// the progress bar.
	Done   int `json:"done"`
	Total  int `json:"total"`
	Failed int `json:"failed"`
	// StartedAt is when the current (or last) reparse began.
	StartedAt time.Time `json:"started_at,omitempty"`
}

// Options selects what a reparse covers.
type Options struct {
	// Agent limits the reparse to one agent (claude|codex|pi); empty reparses all.
	// A partial (agent-filtered) reparse never advances the stored epoch, since it
	// does not bring the whole corpus up to the current parser output.
	Agent string
	// Force marks an operator-initiated run (the CLI or the admin button) as opposed
	// to the startup auto-run. It is informational (it only flavors the log line):
	// the service runs whenever Trigger or Run is called, gated only by the in-process
	// guard and the advisory lock.
	Force bool
}

// Result is the outcome of a synchronous reparse, for the CLI to report.
type Result struct {
	Status
	// SweptBlobs is how many orphaned CAS blobs the post-reparse sweep reclaimed.
	SweptBlobs int
}

// Service runs reparses for one akari-server process. Exactly one reparse runs at
// a time within the process (the running flag), and at most one across processes
// (a Postgres advisory lock).
type Service struct {
	// baseCtx outlives any single HTTP request so a reparse kicked off by the admin
	// button keeps running after the request returns. It is the server's root
	// context, so shutdown cancels an in-flight reparse the same way it winds down
	// the blob sweep.
	baseCtx context.Context
	st      *store.Store

	// pageSize bounds the reparse target page; it seeds from defaultReparsePageSize
	// and is set only at construction, so it carries no lock. fleetTTL likewise seeds
	// from defaultFleetCacheTTL and is construction-only.
	pageSize int
	fleetTTL time.Duration

	mu         sync.Mutex
	status     Status
	onProgress func(Status)

	// fleetCheckedAt/fleetHeld cache the cross-process advisory-lock check so the UI
	// gate does not query the database on every parsed-page request.
	fleetCheckedAt time.Time
	fleetHeld      bool

	// wg tracks the goroutine of any in-flight reparse so Wait can block shutdown
	// until it winds down, before the connection pool closes.
	wg sync.WaitGroup
}

// New builds a Service. baseCtx should be the server's root context so background
// reparses (the startup auto-run and the admin button) are cancelled on shutdown;
// the CLI passes context.Background() to Run directly and does not rely on it.
func New(baseCtx context.Context, st *store.Store) *Service {
	return &Service{baseCtx: baseCtx, st: st, pageSize: defaultReparsePageSize, fleetTTL: defaultFleetCacheTTL}
}

// SetProgressHook registers a callback invoked with the latest Status whenever it
// changes, so the HTTP layer can push progress over SSE. It is meant to be set
// once at construction, before any reparse runs.
func (s *Service) SetProgressHook(fn func(Status)) {
	s.mu.Lock()
	s.onProgress = fn
	s.mu.Unlock()
}

// Status returns the current reparse status.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// SetStatusForTest overrides the reported status without running a reparse. It
// exists so tests in other packages (the HTTP gating tests) can force an
// in-progress state and exercise the gate; it is not used in production.
func (s *Service) SetStatusForTest(st Status) {
	s.mu.Lock()
	s.status = st
	s.mu.Unlock()
}

// Trigger starts a reparse in the background and returns immediately with the
// current status. If one is already running it is a no-op that returns the running
// status rather than starting a second. The background run uses the service's base
// context, so it survives the request that started it and is cancelled on shutdown.
func (s *Service) Trigger(opts Options) Status {
	if !s.begin() {
		// Already running: do not start a second. The caller sees the live status.
		return s.Status()
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if _, err := s.execute(s.baseCtx, opts); err != nil {
			log.Printf("reparse: %v", err)
		}
	}()
	return s.Status()
}

// Run reparses synchronously and returns the result, for the CLI. It honors the
// same in-process guard and advisory lock as Trigger.
func (s *Service) Run(ctx context.Context, opts Options) (Result, error) {
	if !s.begin() {
		// A second concurrent reparse in this process is a no-op; for the CLI (the
		// only Run caller, single-shot) this never happens.
		return Result{Status: s.Status()}, nil
	}
	s.wg.Add(1)
	defer s.wg.Done()
	return s.execute(ctx, opts)
}

// Wait blocks until no reparse is running. The server calls it on shutdown, after
// cancelling the root context (which stops the reparse loop), so an in-flight
// reparse winds down cleanly before the connection pool closes.
func (s *Service) Wait() { s.wg.Wait() }

// begin marks a reparse started, returning false if one is already in progress.
// Setting InProgress here (before the advisory lock is resolved) is deliberate: it
// makes a concurrent Trigger in the same process a no-op and gates the UI from the
// instant a run is accepted.
func (s *Service) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.InProgress {
		return false
	}
	s.status = Status{InProgress: true, StartedAt: time.Now()}
	return true
}

// execute does the actual reparse under the advisory lock and always clears the
// in-progress flag when it returns. It is entered only after begin has reserved the
// run.
func (s *Service) execute(ctx context.Context, opts Options) (Result, error) {
	defer s.finish()
	s.emit()

	// Serialize across processes: if another instance holds the lock, skip rather
	// than reparse the same corpus twice. The follower still gates its parsed UI for
	// the duration via the shared advisory-lock check (see FleetStatus), so it never
	// serves the half-rebuilt rows the holder is writing.
	lock, ok, err := s.st.AcquireReparseLock(ctx)
	if err != nil {
		return Result{Status: s.Status()}, err
	}
	if !ok {
		log.Printf("reparse: another instance holds the reparse lock, skipping")
		return Result{Status: s.Status()}, nil
	}
	defer s.releaseLock(ctx, lock)

	source := "startup"
	if opts.Force {
		source = "operator"
	}
	log.Printf("reparse: starting (%s, agent=%q)", source, opts.Agent)

	total, maxID, err := s.st.ReparseScope(ctx, opts.Agent)
	if err != nil {
		return Result{Status: s.Status()}, err
	}
	s.setTotal(total)

	// Page through targets by id rather than loading the whole list, so peak memory
	// is one page plus the current session even on a corpus of any size.
	var done, failed int
	var afterID int64
	for ctx.Err() == nil {
		page, err := s.st.SessionsForReparsePage(ctx, opts.Agent, afterID, maxID, s.pageSize)
		if err != nil {
			return Result{Status: s.Status()}, err
		}
		if len(page) == 0 {
			break
		}
		for _, t := range page {
			if ctx.Err() != nil {
				break
			}
			if _, err := parse.Reparse(ctx, s.st, t.ID, t.Agent); err != nil {
				failed++
				log.Printf("reparse: session %d (%s): %v", t.ID, t.Agent, err)
			}
			done++
			afterID = t.ID
			s.advance(done, failed)
		}
	}
	if ctx.Err() != nil {
		log.Printf("reparse: cancelled after %d/%d session(s)", done, total)
	}

	// A parser change can rewrite tool bodies and image attachments, orphaning their
	// old blobs, so sweep once the projections are rebuilt. ctx flows in, so a
	// shutdown cancellation that lands during the sweep stops it promptly; the
	// periodic background sweep reclaims any orphans a partial pass left.
	var swept int
	if ctx.Err() == nil {
		swept, err = s.st.SweepBlobs(ctx)
		if err != nil {
			return Result{Status: s.Status()}, err
		}
	}

	// Record the epoch only after a full (all-agents) pass that ran to completion.
	// A cancelled or agent-filtered run leaves the epoch behind so the next startup
	// finishes the job. Failed individual sessions do not block the epoch write: they
	// failed in the parser and re-running converges to the same failure, so blocking
	// would re-trigger a full reparse on every restart forever.
	if ctx.Err() == nil && opts.Agent == "" {
		if err := s.st.SetReparsedEpoch(ctx, parse.Epoch); err != nil {
			return Result{Status: s.Status(), SweptBlobs: swept}, err
		}
	}

	// The returned result reflects the completed run. InProgress is forced false
	// here because the deferred finish that clears it has not run yet at the point
	// this return value is evaluated.
	final := s.Status()
	final.InProgress = false
	log.Printf("reparse: done (%d ok, %d failed of %d; swept %d orphaned blob(s))",
		final.Done-final.Failed, final.Failed, final.Total, swept)
	return Result{Status: final, SweptBlobs: swept}, nil
}

// finish clears the in-progress flag and emits the terminal status. It also voids
// the fleet cache: the advisory lock has been released by the time finish runs, so
// the next gate check should re-read it rather than trust a stale "held".
func (s *Service) finish() {
	s.mu.Lock()
	s.status.InProgress = false
	s.fleetCheckedAt = time.Time{}
	s.mu.Unlock()
	s.emit()
}

// releaseLock unlocks the advisory lock under a bounded, cancellation-detached
// context, so a stuck unlock during shutdown cannot block Wait (and thus the pool
// close) indefinitely.
func (s *Service) releaseLock(ctx context.Context, lock *store.ReparseLock) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reparseUnlockTimeout)
	defer cancel()
	lock.Release(rctx)
}

// FleetStatus is Status with InProgress widened to reflect a reparse running on any
// instance, not just this process. It is what the UI gate and the status endpoint
// read, so a server that is not itself reparsing still gates its parsed pages while
// another instance rebuilds the shared projection. The progress counts stay this
// process's: an observing instance reports the gate without counts, which the UI
// renders as an indeterminate bar.
func (s *Service) FleetStatus(ctx context.Context) Status {
	st := s.Status()
	if !st.InProgress && s.fleetLockHeld(ctx) {
		st.InProgress = true
	}
	return st
}

// fleetLockHeld reports whether any instance holds the reparse advisory lock,
// caching the answer for a short TTL so the gating hot path does not query the
// database on every request. A check error is treated as "not held" so a transient
// database blip fails open (serving pages) rather than wedging the UI gated.
func (s *Service) fleetLockHeld(ctx context.Context) bool {
	s.mu.Lock()
	if !s.fleetCheckedAt.IsZero() && time.Since(s.fleetCheckedAt) < s.fleetTTL {
		held := s.fleetHeld
		s.mu.Unlock()
		return held
	}
	s.mu.Unlock()

	held, err := s.st.ReparseLockHeld(ctx)
	if err != nil {
		return false
	}
	s.mu.Lock()
	s.fleetHeld = held
	s.fleetCheckedAt = time.Now()
	s.mu.Unlock()
	return held
}

func (s *Service) setTotal(total int) {
	s.mu.Lock()
	s.status.Total = total
	s.mu.Unlock()
	s.emit()
}

func (s *Service) advance(done, failed int) {
	s.mu.Lock()
	s.status.Done = done
	s.status.Failed = failed
	s.mu.Unlock()
	s.emit()
}

// emit pushes the current status to the progress hook, if one is set. The hook is
// copied and the status snapshotted under the lock, then called outside it so a
// slow hook (JSON marshal, channel fan-out) never serializes the reparse loop.
func (s *Service) emit() {
	s.mu.Lock()
	hook := s.onProgress
	status := s.status
	s.mu.Unlock()
	if hook != nil {
		hook(status)
	}
}
