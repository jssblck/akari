package httpapi

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/web"
)

// The fleet /insights page serves from a precomputed snapshot that covers every
// trailing window at once, recomputed on a fixed cadence rather than lazily per range.
//
// The page used to memoize each range independently for 60 seconds, computed on the
// first request past the TTL. That kept the page fast but let the range views drift:
// each window was computed at its own instant against its own MVCC snapshot, so two
// views could disagree about a fact both display. Recomputing every window in one
// store pass (store.InsightsRanges: one clock, one exported snapshot) makes the views
// agree by construction; the cadence then only decides how stale the whole set is,
// and it goes stale together.
//
// Hourly is deliberate: the page exists for a team lead reading trends over days to a
// year, where an hour of staleness is invisible, and the full five-window pass costs a
// handful of rollup reads since the insights materialization landed. The snapshot also
// recomputes as soon as a fleet reparse finishes (kickRefresh), so a corpus rewrite
// does not serve pre-reparse figures for the rest of the hour. The process holds the
// snapshot in memory, so a new binary (the only thing that changes parser or scoring
// output, gated by parse.Epoch) starts empty and cannot serve a cross-epoch snapshot.

// insightsRefreshTimeout is the hard ceiling on one refresh pass. The pass is detached
// from any single caller's context (a cold-start reader navigating away must not abort
// the pass other waiters are blocked on, and the background loop must survive shutdown
// races cleanly), so it needs its own bound or a hung database read would wedge every
// later pass behind the singleflight. It sits far above the sub-second warm cost of
// the five-window pass over the rollups, so it only ever cuts off a read that has
// genuinely stalled.
const insightsRefreshTimeout = 2 * time.Minute

// insightsComputeAll is the refresh pass: it computes the snapshot for every fleet
// range in one call. It is injected so the refresher stays free of the store's filter
// construction and is unit-testable without a database.
type insightsComputeAll func(ctx context.Context) (map[string]store.Insights, error)

// insightsSnapshot is one generation of the fleet page: every range's Insights,
// computed in one pass at one instant. The map is replaced whole and never mutated,
// so readers see either the previous generation or the new one, never a mix.
type insightsSnapshot struct {
	byRange map[string]store.Insights
	at      time.Time
}

// insightsRefresher owns the snapshot: it serves reads from the current generation
// and coalesces every recompute (background tick, post-reparse kick, cold start)
// through one singleflight so concurrent triggers run one store pass.
type insightsRefresher struct {
	compute insightsComputeAll

	mu   sync.Mutex
	snap *insightsSnapshot // nil until the first successful pass

	sf   singleflight.Group
	kick chan struct{}
}

func newInsightsRefresher(compute insightsComputeAll) *insightsRefresher {
	return &insightsRefresher{compute: compute, kick: make(chan struct{}, 1)}
}

// get returns rangeKey's slice of the current snapshot and the instant the snapshot
// was computed. Age does not matter here: a stale snapshot is served as-is, because
// replacing it is the background loop's job, not the reader's. Only a cold start (no
// successful pass yet) computes on the read path, and concurrent cold readers
// coalesce onto one pass.
func (r *insightsRefresher) get(ctx context.Context, rangeKey string) (store.Insights, time.Time, error) {
	r.mu.Lock()
	snap := r.snap
	r.mu.Unlock()

	if snap == nil {
		if err := r.refresh(ctx); err != nil {
			return store.Insights{}, time.Time{}, err
		}
		r.mu.Lock()
		snap = r.snap
		r.mu.Unlock()
	}
	ins, ok := snap.byRange[rangeKey]
	if !ok {
		// The handler normalizes the range through web.ParseRange and the pass computes
		// every web.DateRanges key, so this is a wiring bug, not a user input.
		return store.Insights{}, time.Time{}, fmt.Errorf("insights snapshot has no range %q", rangeKey)
	}
	return ins, snap.at, nil
}

// refresh runs one pass and installs the result as the new snapshot. Concurrent calls
// coalesce: whoever arrives while a pass is in flight waits for that pass instead of
// starting another. The pass runs detached from the triggering caller's cancellation
// (keeping its values) under its own timeout, so a reader that navigates away
// mid-flight does not abort the pass for the waiters that remain; the caller itself
// still unblocks on its own context. A failed pass installs nothing, so readers keep
// the previous generation and the next trigger retries.
func (r *insightsRefresher) refresh(ctx context.Context) error {
	ch := r.sf.DoChan("refresh", func() (any, error) {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), insightsRefreshTimeout)
		defer cancel()
		byRange, err := r.compute(cctx)
		if err != nil {
			return nil, err
		}
		r.mu.Lock()
		r.snap = &insightsSnapshot{byRange: byRange, at: time.Now()}
		r.mu.Unlock()
		return nil, nil
	})
	select {
	case <-ctx.Done():
		// This caller gave up; the pass keeps running for the other waiters and still
		// installs the snapshot, so only this trigger returns early.
		return ctx.Err()
	case res := <-ch:
		return res.Err
	}
}

// kickRefresh nudges the background loop to recompute now. It never blocks: the
// channel holds one pending nudge, and a second kick while one is pending folds into
// it (the loop will recompute once, from the latest corpus, which covers both).
func (r *insightsRefresher) kickRefresh() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// run is the background cadence: a warm pass at startup, then a recompute every
// interval and on every kick, until ctx is cancelled. An interval of zero disables
// the proactive tick (and the startup warm pass): the snapshot then computes on first
// request and on kicks only. busy reports whether a fleet reparse is draining; a tick
// that fires mid-drain is skipped, because the corpus is half rewritten and the
// finish kick recomputes the moment the drain ends. Pass failures are logged and the
// previous snapshot keeps serving.
func (r *insightsRefresher) run(ctx context.Context, interval time.Duration, busy func() bool) {
	refresh := func() {
		if busy != nil && busy() {
			return
		}
		if err := r.refresh(ctx); err != nil && ctx.Err() == nil {
			log.Printf("insights refresh: %v", err)
		}
	}

	var tick <-chan time.Time
	if interval > 0 {
		t := time.NewTicker(interval)
		defer t.Stop()
		tick = t.C
		refresh()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick: // nil when the proactive cadence is disabled; never fires
		case <-r.kick:
		}
		refresh()
	}
}

// RunInsightsRefresher runs the fleet insights snapshot's background cadence until
// ctx is cancelled. The server main loop runs it beside the other background loops;
// serving works without it (a cold read computes on demand), it just loses the
// proactive recompute.
func (s *Server) RunInsightsRefresher(ctx context.Context) {
	s.insights.run(ctx, s.Cfg.InsightsRefreshInterval, func() bool {
		return s.worker.Status().InProgress
	})
}

// computeFleetInsights is the real refresh pass: every trailing window the range
// selector offers, computed in one InsightsRanges call so every window reads the
// same corpus state under one clock.
func computeFleetInsights(ctx context.Context, st *store.Store) (map[string]store.Insights, error) {
	now := time.Now()
	filters := make([]store.AnalyticsFilter, len(web.DateRanges))
	for i, dr := range web.DateRanges {
		filters[i] = store.AnalyticsFilter{
			Since:  web.RangeSince(dr.Key, now),
			Bucket: web.TrendBucket(dr.Key),
		}
	}
	outs, err := st.InsightsRanges(ctx, filters, store.AllInsightsPanels)
	if err != nil {
		return nil, err
	}
	byRange := make(map[string]store.Insights, len(outs))
	for i, dr := range web.DateRanges {
		byRange[dr.Key] = outs[i]
	}
	return byRange, nil
}
