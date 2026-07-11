// Package requestbudget bounds expensive request work across an akari process.
package requestbudget

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

const (
	// DefaultCapacity is sixteen 8 MiB-equivalent work units. It permits two
	// concurrent Argon2 operations or one maximum-sized MCP spool with room for
	// lighter database work.
	DefaultCapacity int64 = 16
	// DefaultWaitTimeout lets normal bursts drain through the queue while bounding
	// how long an overloaded request occupies a connection.
	DefaultWaitTimeout = 5 * time.Second
)

// WorkClass is one measured class of expensive request work. The closed constants
// keep names and weights coupled so a caller cannot admit work under a misleading
// metric label or an accidentally smaller weight.
type WorkClass uint8

// Password work is deliberately absent: request-triggered Argon2 hashing is
// bounded by its own fixed worker pool (see httpapi's passwordWork), and a
// second gate here would let budget pressure produce a login response that
// differs from the uniform invalid-credentials path.
const (
	// MCPSpool reserves approximately the 100 MiB request ceiling owned by #134.
	// Applying it to MCP POSTs now also bounds the SDK's current request buffering.
	MCPSpool WorkClass = iota + 1
	PublicAnalytics
	OAuthRegistration
)

var allClasses = [...]WorkClass{MCPSpool, PublicAnalytics, OAuthRegistration}

// MinCapacity is the smallest useful capacity because the heaviest class must be
// able to run once. Smaller values would leave MCP POSTs queued until timeout.
const MinCapacity = int64(12)

var (
	// ErrWaitTimeout means the budget could not admit work within its bounded wait.
	ErrWaitTimeout = errors.New("request budget wait timeout")
	// ErrInvalidClass means a caller supplied a class the budget cannot safely use.
	ErrInvalidClass = errors.New("invalid request budget work class")
)

var waitBuckets = [...]float64{0.001, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

type classMetrics struct {
	queued          int64
	inUseWeight     int64
	acquired        uint64
	rejectedTimeout uint64
	rejectedCancel  uint64
	waitCount       uint64
	waitSum         float64
	waitBuckets     [len(waitBuckets)]uint64
}

// Budget is a fair, process-wide weighted admission queue. Its semaphore is FIFO,
// so lighter work cannot repeatedly jump ahead of an older expensive request.
type Budget struct {
	capacity int64
	wait     time.Duration
	sem      *semaphore.Weighted

	mu      sync.Mutex
	metrics map[string]*classMetrics
}

// New constructs a weighted budget. Capacity must be large enough to admit the
// heaviest built-in work class at least once.
func New(capacity int64, wait time.Duration) (*Budget, error) {
	if capacity < MinCapacity {
		return nil, fmt.Errorf("request budget capacity must be at least %d", MinCapacity)
	}
	if wait <= 0 {
		return nil, fmt.Errorf("request budget wait timeout must be positive")
	}
	b := &Budget{
		capacity: capacity,
		wait:     wait,
		sem:      semaphore.NewWeighted(capacity),
		metrics:  make(map[string]*classMetrics, len(allClasses)),
	}
	for _, class := range allClasses {
		name, _, _ := class.spec()
		b.metrics[name] = &classMetrics{}
	}
	return b, nil
}

// Acquire waits for class capacity or returns ErrWaitTimeout. A cancellation from
// the caller is returned unchanged. The returned release function is idempotent so
// cleanup remains safe when error paths converge.
func (b *Budget) Acquire(ctx context.Context, class WorkClass) (func(), error) {
	name, weight, ok := class.spec()
	if !ok || weight > b.capacity {
		return nil, ErrInvalidClass
	}

	started := time.Now()
	b.mu.Lock()
	m := b.metric(name)
	m.queued++
	b.mu.Unlock()

	waitCtx, cancel := context.WithTimeout(ctx, b.wait)
	err := b.sem.Acquire(waitCtx, weight)
	cancel()
	waited := time.Since(started).Seconds()

	b.mu.Lock()
	m.queued--
	m.waitCount++
	m.waitSum += waited
	for i, upper := range waitBuckets {
		if waited <= upper {
			m.waitBuckets[i]++
		}
	}
	if err == nil {
		m.inUseWeight += weight
		m.acquired++
	} else if ctx.Err() != nil {
		m.rejectedCancel++
	} else {
		m.rejectedTimeout++
	}
	b.mu.Unlock()

	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrWaitTimeout
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			m.inUseWeight -= weight
			b.mu.Unlock()
			b.sem.Release(weight)
		})
	}, nil
}

func (c WorkClass) spec() (string, int64, bool) {
	switch c {
	case MCPSpool:
		return "mcp_spool", 12, true
	case PublicAnalytics:
		return "public_analytics", 4, true
	case OAuthRegistration:
		return "oauth_registration", 1, true
	default:
		return "", 0, false
	}
}

func (b *Budget) metric(name string) *classMetrics {
	m := b.metrics[name]
	if m == nil {
		m = &classMetrics{}
		b.metrics[name] = m
	}
	return m
}

// ServeHTTP exposes the queue in Prometheus text format. The metrics contain no
// request identity or route parameters and are safe for a normal metrics scraper.
func (b *Budget) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(b.metricsText()))
}

func (b *Budget) metricsText() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	names := make([]string, 0, len(b.metrics))
	for name := range b.metrics {
		names = append(names, name)
	}
	sort.Strings(names)

	var out strings.Builder
	out.WriteString("# HELP akari_request_budget_capacity_weight Configured process-wide weighted capacity.\n")
	out.WriteString("# TYPE akari_request_budget_capacity_weight gauge\n")
	fmt.Fprintf(&out, "akari_request_budget_capacity_weight %d\n", b.capacity)
	writeMetricHeader(&out, "akari_request_budget_queue_depth", "Requests currently waiting for admission.", "gauge")
	writeMetricHeader(&out, "akari_request_budget_in_use_weight", "Weighted capacity currently held by each work class.", "gauge")
	writeMetricHeader(&out, "akari_request_budget_utilization_ratio", "Fraction of process capacity currently held by each work class.", "gauge")
	writeMetricHeader(&out, "akari_request_budget_acquired_total", "Requests admitted by work class.", "counter")
	writeMetricHeader(&out, "akari_request_budget_rejected_total", "Requests rejected before admission by work class and reason.", "counter")
	writeMetricHeader(&out, "akari_request_budget_wait_seconds", "Time requests spent waiting for weighted admission.", "histogram")
	for _, name := range names {
		m := b.metrics[name]
		label := strconv.Quote(name)
		fmt.Fprintf(&out, "akari_request_budget_queue_depth{class=%s} %d\n", label, m.queued)
		fmt.Fprintf(&out, "akari_request_budget_in_use_weight{class=%s} %d\n", label, m.inUseWeight)
		fmt.Fprintf(&out, "akari_request_budget_utilization_ratio{class=%s} %g\n", label, float64(m.inUseWeight)/float64(b.capacity))
		fmt.Fprintf(&out, "akari_request_budget_acquired_total{class=%s} %d\n", label, m.acquired)
		fmt.Fprintf(&out, "akari_request_budget_rejected_total{class=%s,reason=\"timeout\"} %d\n", label, m.rejectedTimeout)
		fmt.Fprintf(&out, "akari_request_budget_rejected_total{class=%s,reason=\"canceled\"} %d\n", label, m.rejectedCancel)
		for i, upper := range waitBuckets {
			fmt.Fprintf(&out, "akari_request_budget_wait_seconds_bucket{class=%s,le=%q} %d\n", label, strconv.FormatFloat(upper, 'g', -1, 64), m.waitBuckets[i])
		}
		fmt.Fprintf(&out, "akari_request_budget_wait_seconds_bucket{class=%s,le=\"+Inf\"} %d\n", label, m.waitCount)
		fmt.Fprintf(&out, "akari_request_budget_wait_seconds_sum{class=%s} %g\n", label, m.waitSum)
		fmt.Fprintf(&out, "akari_request_budget_wait_seconds_count{class=%s} %d\n", label, m.waitCount)
	}
	return out.String()
}

func writeMetricHeader(out *strings.Builder, name, help, kind string) {
	fmt.Fprintf(out, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}
