package httpapi

import (
	"context"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/store"
)

// insightsTTL bounds how long a computed fleet Insights snapshot is reused before
// the next reader recomputes it. The fleet /insights page runs on the order of a
// dozen aggregate queries over the whole trailing window, which is the several
// seconds a first load takes; caching the result collapses every subsequent load
// inside the window to a map lookup.
//
// A time bound, not per-write invalidation, is the right key here. The corpus
// changes whenever any session rebuilds, which on an active fleet is every few
// seconds, so invalidating on each rebuild would keep the cache permanently cold
// and buy nothing. A fleet-wide dashboard reading a minute stale is invisible to
// the team lead it is for: the bars do not perceptibly move in a minute. The
// process holds the cache, so a new binary (the only thing that changes parser or
// scoring output, gated by parse.Epoch) starts empty and cannot serve a
// cross-epoch-stale snapshot; the epoch rides the key as a second guard.
const insightsTTL = 60 * time.Second

// insightsComputeTimeout is the hard ceiling on one shared compute. The compute is
// detached from any single caller's context (so one reader navigating away does not
// abort the query the other waiters are blocked on), which means it needs its own
// bound or a hung database read would park every waiter until the process restarts.
// It sits well above the several seconds a cold load takes, so it never cuts off a
// legitimate first render, only a read that has genuinely stalled.
const insightsComputeTimeout = 45 * time.Second

// insightsCompute is the miss path: it runs the real store query for a range. It
// is injected so the cache stays free of the handler's filter construction and is
// unit-testable without a database.
type insightsCompute func(ctx context.Context) (store.Insights, error)

type insightsCacheEntry struct {
	ins store.Insights
	at  time.Time
}

// insightsCache memoizes the fleet Insights snapshot per trailing-window range for
// insightsTTL. Concurrent misses on the same key coalesce through a singleflight
// group (the same pattern the OG-card renderers use), so a burst of loads on a
// cold key runs one query rather than one per request.
type insightsCache struct {
	mu sync.Mutex
	m  map[string]insightsCacheEntry
	sf singleflight.Group
}

func newInsightsCache() *insightsCache {
	return &insightsCache{m: make(map[string]insightsCacheEntry)}
}

// cacheKey namespaces a range key by the parser epoch, so a same-process epoch (it
// never changes at runtime, but the pairing documents the invariant) or a future
// multi-epoch reader cannot cross a stale snapshot into a fresh one.
func cacheKey(rangeKey string) string {
	return strconv.Itoa(parse.Epoch) + "|" + rangeKey
}

// load returns the cached snapshot for rangeKey when it is still fresh, otherwise
// computes it once (coalescing concurrent callers) and caches it. The shared compute
// runs detached from any single caller's context so one reader navigating away
// mid-flight does not abort the query the other waiters are blocked on, but it is
// bounded by insightsComputeTimeout so a stalled read cannot park waiters forever.
// Waiters block through DoChan and select on their OWN context, so a client that
// disconnects returns immediately (with ctx.Err()) rather than staying parked until
// the detached compute finishes: the compute keeps running to warm the cache for the
// readers that remain. now is passed in so tests drive the clock.
func (c *insightsCache) load(ctx context.Context, rangeKey string, now time.Time, compute insightsCompute) (store.Insights, error) {
	key := cacheKey(rangeKey)

	c.mu.Lock()
	if e, ok := c.m[key]; ok && now.Sub(e.at) <= insightsTTL {
		c.mu.Unlock()
		return e.ins, nil
	}
	c.mu.Unlock()

	ch := c.sf.DoChan(key, func() (any, error) {
		// Re-check under the flight: a caller that queued behind the leader while it
		// computed should read the value the leader just stored, not recompute.
		c.mu.Lock()
		if e, ok := c.m[key]; ok && now.Sub(e.at) <= insightsTTL {
			c.mu.Unlock()
			return e.ins, nil
		}
		c.mu.Unlock()

		// Detach from the triggering caller's cancellation (keep its values), then cap the
		// read so a hung database call cannot hold the flight open indefinitely.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), insightsComputeTimeout)
		defer cancel()
		ins, err := compute(cctx)
		if err != nil {
			return store.Insights{}, err
		}
		c.mu.Lock()
		c.m[key] = insightsCacheEntry{ins: ins, at: now}
		c.mu.Unlock()
		return ins, nil
	})

	select {
	case <-ctx.Done():
		// This caller gave up (disconnect or its own deadline); the flight keeps running for
		// the remaining waiters and warms the cache, so only this request returns early.
		return store.Insights{}, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return store.Insights{}, res.Err
		}
		return res.Val.(store.Insights), nil
	}
}
