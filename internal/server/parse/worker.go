package parse

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// duePageSize bounds one due-scan query and the run of rebuilds behind it. The
// drain pages by session id, so peak memory is one page plus the sessions being
// rebuilt, not the whole backlog. It is a var so tests can shrink it to
// exercise the multi-page drain.
var duePageSize = 256

// defaultFleetCacheTTL is how long a positive cross-instance "a fleet rebuild
// is draining" answer is cached, so gating a parsed-page request does not probe
// the epoch-stale set on every request. The negative answer is never cached (see
// FleetStatus), mirroring the old advisory-lock gate's asymmetry: briefly
// over-gating after a rebuild finishes is safe, serving mixed pages is not.
const defaultFleetCacheTTL = 2 * time.Second

// Status is a snapshot of the current (or last) fleet rebuild. It is what the
// HTTP status endpoint and the SSE progress stream serialize, so its JSON tags
// are the wire contract the frontend reads. Ordinary live-session rebuilds (a
// chunk landing) never set InProgress: the gate exists for the corpus-wide mix
// an epoch rollout creates, not for steady-state ingest.
type Status struct {
	// InProgress is true while epoch-stale sessions are draining. The parsed UI
	// gates on it.
	InProgress bool `json:"in_progress"`
	// Done and Total track the epoch-stale backlog: Total is the backlog size
	// (grown when sessions announced mid-drain join it), Done is how much of it
	// has drained, and Failed counts deterministic parser failures along the
	// way. Done/Total drive the progress bar.
	Done   int `json:"done"`
	Total  int `json:"total"`
	Failed int `json:"failed"`
	// StartedAt is when the current (or last) fleet rebuild began.
	StartedAt time.Time `json:"started_at,omitempty"`
}

// Worker owns every projection write in the server: it drains due sessions
// (bytes ahead of the last rebuild, or a stale parser epoch) through
// RebuildSession, woken in-process by the ingest chunk handler and backstopped
// by a periodic maintenance tick that also grades settled sessions. Multiple
// instances need no coordination: two rebuilds of one session serialize on its
// row locks, same-epoch rebuilds are identical so whichever commits last wins,
// and across a rolling deploy the monotonic due predicate (store.DueSessions)
// keeps an older binary off sessions a newer one already stamped.
type Worker struct {
	st          *store.Store
	concurrency int
	settleEvery time.Duration
	fleetTTL    time.Duration

	// wake carries at most one pending signal: a chunk that lands mid-drain
	// leaves a buffered wake, so the loop drains again and picks it up. The wake
	// buys latency, not correctness (the byte comparison is what marks the
	// session due).
	wake chan struct{}

	mu sync.Mutex
	// onRebuilt is called after each successful rebuild commits, with the
	// session id, so the HTTP layer can push an SSE refresh to watching
	// browsers. onStatus is called whenever the fleet-rebuild status changes,
	// for the progress stream. Both are guarded by mu: they are set at wiring
	// time, but the worker may already be draining (a fresh migration makes
	// every session due at boot), so the setters race the drain's reads
	// without the lock.
	onRebuilt      func(sessionID int64)
	onStatus       func(Status)
	status         Status
	fleetCheckedAt time.Time
	fleetStale     bool
}

// NewWorker builds a Worker draining through st with the given rebuild
// concurrency. settleEvery is the maintenance tick (zero disables it; the
// worker then drains only on wakes).
func NewWorker(st *store.Store, concurrency int, settleEvery time.Duration) *Worker {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Worker{
		st:          st,
		concurrency: concurrency,
		settleEvery: settleEvery,
		fleetTTL:    defaultFleetCacheTTL,
		wake:        make(chan struct{}, 1),
	}
}

// SetRebuiltHook registers the per-session post-rebuild callback (the SSE
// publish). Set once at wiring time.
func (w *Worker) SetRebuiltHook(fn func(sessionID int64)) {
	w.mu.Lock()
	w.onRebuilt = fn
	w.mu.Unlock()
}

// SetStatusHook registers the fleet-rebuild progress callback. Set once at
// wiring time.
func (w *Worker) SetStatusHook(fn func(Status)) {
	w.mu.Lock()
	w.onStatus = fn
	w.mu.Unlock()
}

// rebuiltHook snapshots the rebuilt callback under the lock; rebuildBatch calls
// it outside the lock so a slow SSE publish never serializes the pool.
func (w *Worker) rebuiltHook() func(sessionID int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.onRebuilt
}

// Wake nudges the worker to drain. It never blocks: the channel holds one
// pending signal, and a drain is already guaranteed for anything the buffered
// signal covers.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Trigger marks a scope (one agent, or everything when agent is empty) due for
// a rebuild and wakes the worker. It is the admin Reparse button and the
// reparse CLI: a fleet rebuild is not a separate mechanism, just the ordinary
// drain over a scope forced due. It returns how many sessions were marked.
func (w *Worker) Trigger(ctx context.Context, agent string) (int, error) {
	n, err := w.st.MarkEpochStale(ctx, agent)
	if err != nil {
		return 0, err
	}
	w.Wake()
	return n, nil
}

// Run drains until ctx is cancelled. Call it once, on its own goroutine; the
// caller waits for it to return on shutdown before closing the pool. It drains
// immediately on start, which is what rolls a bumped epoch (or a fresh
// migration, where every session reads as due) out without any separate
// startup check.
func (w *Worker) Run(ctx context.Context) {
	var tick <-chan time.Time
	if w.settleEvery > 0 {
		t := time.NewTicker(w.settleEvery)
		defer t.Stop()
		tick = t.C
	}
	w.drainLogged(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
			w.drainLogged(ctx)
		case <-tick:
			w.drainLogged(ctx)
			w.settle(ctx)
		}
	}
}

// drainLogged is the Run loop's form of drain: operational trouble is logged
// and the loop moves on, because the failed sessions stay due and the next
// wake or tick retries them. Only the foreground Drain propagates the error.
func (w *Worker) drainLogged(ctx context.Context) {
	if err := w.drain(ctx); err != nil && ctx.Err() == nil {
		log.Printf("parse worker: drain: %v", err)
	}
}

// Drain runs one synchronous drain over the current due set and returns the
// final status. It is the reparse CLI's foreground path: mark a scope due with
// Trigger or MarkEpochStale, then Drain to completion, no Run loop involved.
// The error reports operational trouble (a due-scan failure, or sessions whose
// rebuild rolled back and stayed due); a foreground caller must fail on it
// rather than report an incomplete drain as done. Deterministic parser
// failures are not errors here: they are recorded per session and counted in
// Status.Failed.
func (w *Worker) Drain(ctx context.Context) (Status, error) {
	err := w.drain(ctx)
	return w.Status(), err
}

// drain rebuilds every currently-due session, paging by id. When the backlog
// includes epoch-stale sessions (a fleet rebuild), it publishes progress for
// the UI gate; ordinary byte-dirty rebuilds run silently. The returned error
// summarizes operational trouble (scan failures, rebuilds that rolled back);
// the affected sessions all stay due, so a retry is always another drain away.
// A context cancellation is not an error: shutdown mid-drain is the designed
// resume point.
func (w *Worker) drain(ctx context.Context) error {
	staleTotal, err := w.st.EpochStaleCount(ctx, Epoch)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("count epoch-stale sessions: %w", err)
	}
	fleet := staleTotal > 0
	if fleet {
		w.beginFleet(staleTotal)
		log.Printf("parse worker: fleet rebuild starting (%d session(s) behind epoch %d)", staleTotal, Epoch)
	}
	// The drain is one forward keyset pass: afterID only grows, so no session is
	// visited twice within it, including sessions that fail operationally (still
	// due, but behind the cursor; the next wake or tick retries them).
	var done, failed, staleDone, opFailed int
	var firstOpErr, scanErr error
	var afterID int64
	for ctx.Err() == nil {
		page, err := w.st.DueSessions(ctx, Epoch, afterID, duePageSize)
		if err != nil {
			if ctx.Err() == nil {
				scanErr = fmt.Errorf("due scan after id %d: %w", afterID, err)
			}
			break
		}
		if len(page) == 0 {
			break
		}
		afterID = page[len(page)-1].ID
		d, f, sd, op, opErr := w.rebuildBatch(ctx, page)
		done += d
		failed += f
		staleDone += sd
		opFailed += op
		if firstOpErr == nil {
			firstOpErr = opErr
		}
		if fleet {
			// Progress counts only the epoch-stale subset: a fleet drain also picks
			// up byte-dirty live sessions, and counting those would run Done past
			// the backlog total counted when the drain began.
			w.advanceFleet(staleDone, failed)
		}
	}
	if fleet {
		w.finishFleet()
		if ctx.Err() == nil {
			log.Printf("parse worker: fleet rebuild done (%d session(s) rebuilt, %d failed)", done, failed)
		} else {
			log.Printf("parse worker: fleet rebuild interrupted after %d rebuild(s); it resumes on next start", done)
		}
	}
	if scanErr != nil {
		return scanErr
	}
	if opFailed > 0 {
		return fmt.Errorf("%d session(s) failed operationally and stay due (first: %w)", opFailed, firstOpErr)
	}
	return nil
}

// rebuildBatch rebuilds one page of due sessions across the worker pool,
// returning how many completed, how many of those were deterministic parser
// failures, how many were epoch-stale (the fleet progress numerator), and how
// many failed operationally (rolled back, still due) along with the first such
// error. Distinct sessions rebuild in parallel; the pool bounds concurrent
// transactions.
func (w *Worker) rebuildBatch(ctx context.Context, batch []store.DueSession) (done, failed, staleDone, opFailed int, firstOpErr error) {
	if len(batch) == 0 {
		return 0, 0, 0, 0, nil
	}
	sem := make(chan struct{}, w.concurrency)
	rebuilt := w.rebuiltHook()
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, t := range batch {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(t store.DueSession) {
			defer wg.Done()
			defer func() { <-sem }()
			err := Rebuild(ctx, w.st, t.ID, t.Agent)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				done++
				if t.EpochStale {
					staleDone++
				}
				if rebuilt != nil {
					rebuilt(t.ID)
				}
			case ctx.Err() != nil:
				// Shutdown landed mid-rebuild; the transaction rolled back and the
				// session stays due for the next start.
			case isParserError(err):
				// Deterministic parser failure: the store recorded the attempt on the
				// failure markers, so the session has left the due set until its bytes
				// or the epoch move. The prior projection survives.
				done++
				failed++
				if t.EpochStale {
					staleDone++
				}
				log.Printf("parse worker: session %d (%s): %v", t.ID, t.Agent, err)
			default:
				// Operational error: the rebuild rolled back and the session stays due.
				// The drain's keyset cursor is already past it, so it is not retried in
				// a hot loop; the next wake or tick retries it.
				opFailed++
				if firstOpErr == nil {
					firstOpErr = fmt.Errorf("session %d (%s): %w", t.ID, t.Agent, err)
				}
				log.Printf("parse worker: session %d (%s): %v", t.ID, t.Agent, err)
			}
		}(t)
	}
	wg.Wait()
	return done, failed, staleDone, opFailed, firstOpErr
}

func isParserError(err error) bool {
	var perr *ParserError
	return errors.As(err, &perr)
}

// settle grades sessions that settled between rebuilds (a rebuild grades a
// session only once it is settled or terminal, so one that settled quietly
// since its last rebuild is graded here). Bounded by its own timeout so a slow
// catch-up cannot stack up behind the tick.
func (w *Worker) settle(ctx context.Context) {
	passCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	n, err := w.st.RefreshSettledSignals(passCtx)
	switch {
	case errors.Is(err, context.Canceled):
		// Shutdown cancelled the pass; Run returns on the next ctx.Done check.
	case errors.Is(err, context.DeadlineExceeded):
		log.Printf("signals settle: pass timed out before draining the due backlog; it resumes next tick")
	case err != nil:
		log.Printf("signals settle: %v", err)
	case n > 0:
		log.Printf("signals settle: refreshed %d session(s)", n)
	}
}

// Status returns this process's fleet-rebuild status.
func (w *Worker) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

// SetStatusForTest overrides the reported status without running a drain. It
// exists so tests in other packages (the HTTP gating tests) can force an
// in-progress state and exercise the gate; it is not used in production.
func (w *Worker) SetStatusForTest(st Status) {
	w.mu.Lock()
	w.status = st
	w.mu.Unlock()
}

// FleetStatus is Status widened to reflect a fleet rebuild draining on any
// instance, not just this process. A follower instance sees the shared
// epoch-stale backlog (the same corpus state the driving instance is draining)
// and gates its parsed pages with an indeterminate bar: the counts stay this
// process's, so only the instance doing the work reports progress numbers.
// Only the positive answer is cached (briefly over-gating after a rebuild
// finishes is safe); the negative is always read fresh, so a follower can
// never serve mixed pages on a stale "not draining". A check error fails open
// (serving pages) rather than wedging the UI gated on a transient blip.
func (w *Worker) FleetStatus(ctx context.Context) Status {
	st := w.Status()
	if st.InProgress {
		return st
	}
	if w.fleetStaleExists(ctx) {
		st.InProgress = true
	}
	return st
}

func (w *Worker) fleetStaleExists(ctx context.Context) bool {
	w.mu.Lock()
	if w.fleetStale && time.Since(w.fleetCheckedAt) < w.fleetTTL {
		w.mu.Unlock()
		return true
	}
	w.mu.Unlock()

	stale, err := w.st.EpochStaleExists(ctx, Epoch)
	if err != nil {
		return false
	}
	w.mu.Lock()
	if stale {
		w.fleetStale = true
		w.fleetCheckedAt = time.Now()
	} else {
		w.fleetStale = false
		w.fleetCheckedAt = time.Time{}
	}
	w.mu.Unlock()
	return stale
}

func (w *Worker) beginFleet(total int) {
	w.mu.Lock()
	w.status = Status{InProgress: true, Total: total, StartedAt: time.Now()}
	w.mu.Unlock()
	w.emit()
}

// advanceFleet publishes fleet progress from completion counts alone: done is
// how many epoch-stale rebuilds this drain has finished against the backlog
// total counted once when it began. Sessions announced mid-drain start at
// parser_epoch 0 with higher ids, so the same keyset pass reaches them; their
// completions can push done past the opening total, and Total grows to cover
// them. Counting completions instead of re-counting the backlog per page keeps
// a fleet drain's progress bookkeeping O(pages) rather than re-scanning the
// stale set every page, and both numbers stay monotone for a watching
// progress stream.
func (w *Worker) advanceFleet(done, failed int) {
	w.mu.Lock()
	if done > w.status.Total {
		w.status.Total = done
	}
	w.status.Done = done
	w.status.Failed = failed
	w.mu.Unlock()
	w.emit()
}

func (w *Worker) finishFleet() {
	w.mu.Lock()
	w.status.InProgress = false
	w.fleetStale = false
	w.fleetCheckedAt = time.Time{}
	w.mu.Unlock()
	w.emit()
}

// emit pushes the current status to the progress hook, if one is set. The hook
// is copied and the status snapshotted under the lock, then called outside it
// so a slow hook (JSON marshal, channel fan-out) never serializes the drain.
func (w *Worker) emit() {
	w.mu.Lock()
	hook := w.onStatus
	status := w.status
	w.mu.Unlock()
	if hook != nil {
		hook(status)
	}
}
