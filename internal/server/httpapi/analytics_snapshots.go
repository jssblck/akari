package httpapi

import (
	"container/list"
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/ogimage"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/web"
)

const (
	analyticsSnapshotRefreshTimeout = 2 * time.Minute
)

type analyticsScopeKind uint8

const (
	analyticsUserScope analyticsScopeKind = iota + 1
	analyticsProjectScope
)

type analyticsScope struct {
	kind analyticsScopeKind
	id   int64
}

func (s analyticsScope) String() string {
	switch s.kind {
	case analyticsUserScope:
		return "user:" + strconv.FormatInt(s.id, 10)
	case analyticsProjectScope:
		return "project:" + strconv.FormatInt(s.id, 10)
	default:
		return "unknown:" + strconv.FormatInt(s.id, 10)
	}
}

type analyticsSnapshotKey struct {
	scope    analyticsScope
	rangeKey string
}

func (k analyticsSnapshotKey) String() string {
	return k.scope.String() + ":" + k.rangeKey
}

// analyticsPageSnapshot is installed whole after a successful refresh and never
// mutated afterward. The cache returns this value only to render paths, which treat
// the store slices as read-only, so concurrent readers share one immutable generation.
type analyticsPageSnapshot struct {
	analytics store.Analytics
	insights  store.Insights
}

type analyticsSnapshotState string

const (
	analyticsSnapshotHit     analyticsSnapshotState = "hit"
	analyticsSnapshotMiss    analyticsSnapshotState = "miss"
	analyticsSnapshotRefresh analyticsSnapshotState = "refresh"
	analyticsSnapshotStale   analyticsSnapshotState = "stale"
)

type analyticsSnapshotMeta struct {
	state analyticsSnapshotState
	at    time.Time
}

type analyticsSnapshotCompute func(context.Context, analyticsSnapshotKey, time.Time) (analyticsPageSnapshot, error)

type analyticsSnapshotEntry struct {
	key      analyticsSnapshotKey
	snapshot analyticsPageSnapshot
	at       time.Time
	lru      *list.Element
}

type analyticsRefreshResult struct {
	snapshot analyticsPageSnapshot
	at       time.Time
	state    analyticsSnapshotState
}

// analyticsSnapshotCache bounds aggregate work in three dimensions: completed
// generations have an LRU entry limit, generations have explicit fresh and stale
// lifetimes, and concurrent refreshes for one exact scope and range share one
// singleflight. Publication invalidation advances a cache-wide generation so an
// already-running refresh cannot reinstall an entry after the access state changed.
type analyticsSnapshotCache struct {
	compute  analyticsSnapshotCompute
	freshFor time.Duration
	staleFor time.Duration
	limit    int
	timeout  time.Duration
	now      func() time.Time
	logf     func(string, ...any)
	// afterFlight is a test barrier used to make concurrent-arrival tests
	// deterministic. Production leaves it nil.
	afterFlight func()

	mu         sync.Mutex
	entries    map[analyticsSnapshotKey]*analyticsSnapshotEntry
	lru        list.List
	generation uint64
	sf         singleflight.Group
}

func newAnalyticsSnapshotCache(freshFor, staleFor time.Duration, limit int, compute analyticsSnapshotCompute) *analyticsSnapshotCache {
	if freshFor <= 0 {
		freshFor = config.DefaultAnalyticsSnapshotFreshness
	}
	if limit <= 0 {
		limit = config.DefaultAnalyticsSnapshotLimit
	}
	return &analyticsSnapshotCache{
		compute:  compute,
		freshFor: freshFor,
		staleFor: staleFor,
		limit:    limit,
		timeout:  analyticsSnapshotRefreshTimeout,
		now:      time.Now,
		logf:     log.Printf,
		entries:  make(map[analyticsSnapshotKey]*analyticsSnapshotEntry, limit),
	}
}

func (c *analyticsSnapshotCache) get(ctx context.Context, key analyticsSnapshotKey) (analyticsPageSnapshot, analyticsSnapshotMeta, error) {
	now := c.now()
	c.mu.Lock()
	entry := c.entries[key]
	if entry != nil && now.Sub(entry.at) < c.freshFor {
		c.lru.MoveToFront(entry.lru)
		result := entry.snapshot
		at := entry.at
		c.mu.Unlock()
		return result, analyticsSnapshotMeta{state: analyticsSnapshotHit, at: at}, nil
	}
	var staleEntry *analyticsSnapshotEntry
	var staleSnapshot analyticsPageSnapshot
	var staleAt time.Time
	if entry != nil && now.Sub(entry.at) < c.freshFor+c.staleFor {
		staleEntry = entry
		staleSnapshot = entry.snapshot
		staleAt = entry.at
	}
	generation := c.generation
	c.mu.Unlock()

	flightKey := key.String() + ":generation:" + strconv.FormatUint(generation, 10)
	flight := c.sf.DoChan(flightKey, func() (any, error) {
		// Another refresh may have completed after the caller's first lookup but before
		// this flight started. Recheck under the cache lock so a scheduler race cannot
		// run a redundant store pass.
		started := c.now()
		c.mu.Lock()
		if current := c.entries[key]; current != nil && started.Sub(current.at) < c.freshFor {
			c.lru.MoveToFront(current.lru)
			result := analyticsRefreshResult{snapshot: current.snapshot, at: current.at, state: analyticsSnapshotHit}
			c.mu.Unlock()
			return result, nil
		}
		c.mu.Unlock()

		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
		defer cancel()
		snapshot, err := c.compute(refreshCtx, key, started)
		if err != nil {
			c.logf("analytics snapshot refresh %s: %v", key, err)
			return nil, err
		}

		state := analyticsSnapshotRefresh
		if entry == nil {
			state = analyticsSnapshotMiss
		}
		completed := c.now()
		c.mu.Lock()
		if c.generation == generation {
			c.installLocked(key, snapshot, completed)
		}
		c.mu.Unlock()
		return analyticsRefreshResult{snapshot: snapshot, at: completed, state: state}, nil
	})
	if c.afterFlight != nil {
		c.afterFlight()
	}

	select {
	case <-ctx.Done():
		return analyticsPageSnapshot{}, analyticsSnapshotMeta{}, ctx.Err()
	case result := <-flight:
		if result.Err == nil {
			refreshed := result.Val.(analyticsRefreshResult)
			return refreshed.snapshot, analyticsSnapshotMeta{state: refreshed.state, at: refreshed.at}, nil
		}
		if staleEntry != nil && c.now().Sub(staleAt) < c.freshFor+c.staleFor {
			c.mu.Lock()
			if current := c.entries[key]; current == staleEntry {
				c.lru.MoveToFront(staleEntry.lru)
			}
			c.mu.Unlock()
			return staleSnapshot, analyticsSnapshotMeta{state: analyticsSnapshotStale, at: staleAt}, nil
		}
		return analyticsPageSnapshot{}, analyticsSnapshotMeta{}, result.Err
	}
}

func (c *analyticsSnapshotCache) installLocked(key analyticsSnapshotKey, snapshot analyticsPageSnapshot, at time.Time) {
	if current := c.entries[key]; current != nil {
		current.snapshot = snapshot
		current.at = at
		c.lru.MoveToFront(current.lru)
		return
	}
	entry := &analyticsSnapshotEntry{key: key, snapshot: snapshot, at: at}
	entry.lru = c.lru.PushFront(entry)
	c.entries[key] = entry
	for len(c.entries) > c.limit {
		oldest := c.lru.Back()
		victim := oldest.Value.(*analyticsSnapshotEntry)
		delete(c.entries, victim.key)
		c.lru.Remove(oldest)
	}
}

// invalidate removes every range for a scope. Advancing generation also prevents
// a refresh already in progress for this or any other scope from installing after
// the access-state write; publication changes are rare, so discarding an unrelated
// in-flight generation is preferable to retaining an unbounded per-scope epoch map.
func (c *analyticsSnapshotCache) invalidate(scope analyticsScope) {
	c.mu.Lock()
	c.generation++
	for key, entry := range c.entries {
		if key.scope == scope {
			delete(c.entries, key)
			c.lru.Remove(entry.lru)
		}
	}
	c.mu.Unlock()
}

func (c *analyticsSnapshotCache) invalidateAll() {
	c.mu.Lock()
	c.generation++
	c.entries = make(map[analyticsSnapshotKey]*analyticsSnapshotEntry, c.limit)
	c.lru.Init()
	c.mu.Unlock()
}

func (s *Server) computeAnalyticsSnapshot(ctx context.Context, key analyticsSnapshotKey, now time.Time) (analyticsPageSnapshot, error) {
	filter := store.AnalyticsFilter{
		Since: web.RangeSince(key.rangeKey, now),
		Until: ogimage.DefaultUntil(now),
	}
	switch key.scope.kind {
	case analyticsUserScope:
		filter.UserIDs = []int64{key.scope.id}
		analytics, err := s.Store.Analytics(ctx, filter)
		if err != nil {
			return analyticsPageSnapshot{}, err
		}
		return analyticsPageSnapshot{analytics: analytics}, nil
	case analyticsProjectScope:
		filter.ProjectID = key.scope.id
		filter.Bucket = web.TrendBucket(key.rangeKey)
		analytics, insights, err := s.Store.ProjectOverviewSnapshot(ctx, filter)
		if err != nil {
			return analyticsPageSnapshot{}, err
		}
		return analyticsPageSnapshot{analytics: analytics, insights: insights}, nil
	default:
		return analyticsPageSnapshot{}, fmt.Errorf("unknown analytics scope kind %d", key.scope.kind)
	}
}

func observeAnalyticsSnapshot(w http.ResponseWriter, started time.Time, meta analyticsSnapshotMeta, freshFor, staleFor time.Duration) {
	age := max(time.Since(meta.at), 0)
	w.Header().Set("X-Akari-Analytics-Snapshot", fmt.Sprintf(
		"state=%s; age=%d; fresh=%d; stale=%d",
		meta.state,
		int64(age/time.Second),
		int64(freshFor/time.Second),
		int64(staleFor/time.Second),
	))
	w.Header().Add("Server-Timing", fmt.Sprintf(
		"analytics;dur=%.1f;desc=%q, analytics-age;dur=%.1f",
		float64(time.Since(started).Microseconds())/1000,
		"snapshot-"+string(meta.state),
		float64(age.Microseconds())/1000,
	))
	if meta.state == analyticsSnapshotStale {
		w.Header().Set("Warning", `110 - "Response is stale"`)
	}
}
