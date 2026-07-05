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
// computes it once (coalescing concurrent callers) and caches it. The compute runs
// under a cancel-detached context so one caller navigating away mid-flight does not
// abort the shared query the other waiters are blocked on; the store's own read
// path bounds it. now is passed in so tests drive the clock.
func (c *insightsCache) load(ctx context.Context, rangeKey string, now time.Time, compute insightsCompute) (store.Insights, error) {
	key := cacheKey(rangeKey)

	c.mu.Lock()
	if e, ok := c.m[key]; ok && now.Sub(e.at) <= insightsTTL {
		c.mu.Unlock()
		return e.ins, nil
	}
	c.mu.Unlock()

	v, err, _ := c.sf.Do(key, func() (any, error) {
		// Re-check under the flight: a caller that queued behind the leader while it
		// computed should read the value the leader just stored, not recompute.
		c.mu.Lock()
		if e, ok := c.m[key]; ok && now.Sub(e.at) <= insightsTTL {
			c.mu.Unlock()
			return e.ins, nil
		}
		c.mu.Unlock()

		ins, err := compute(context.WithoutCancel(ctx))
		if err != nil {
			return store.Insights{}, err
		}
		c.mu.Lock()
		c.m[key] = insightsCacheEntry{ins: ins, at: now}
		c.mu.Unlock()
		return ins, nil
	})
	if err != nil {
		return store.Insights{}, err
	}
	return v.(store.Insights), nil
}
