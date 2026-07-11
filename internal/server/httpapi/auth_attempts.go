package httpapi

import (
	"container/list"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	authAccountAttemptsPerMinute = 120
	authAccountBurst             = 30
	authSourceAttemptsPerMinute  = 600
	authSourceBurst              = 100
	authAttemptKeys              = 4096
)

type tokenBucket struct {
	tokens  float64
	last    time.Time
	element *list.Element
}

// attemptBuckets is a bounded-memory, process-local token bucket collection.
// The ceilings are intentionally abuse-only: Argon2 admission handles ordinary
// bursts, while these buckets stop one account or peer from continuously owning
// that queue.
type attemptBuckets struct {
	mu         sync.Mutex
	byKey      map[string]*tokenBucket
	recency    *list.List
	perSecond  float64
	burst      float64
	maxEntries int
}

type authAttemptLimiter struct {
	accounts *attemptBuckets
	sources  *attemptBuckets
}

func newAuthAttemptLimiter() *authAttemptLimiter {
	return &authAttemptLimiter{
		accounts: newAttemptBuckets(authAccountAttemptsPerMinute, authAccountBurst, authAttemptKeys),
		sources:  newAttemptBuckets(authSourceAttemptsPerMinute, authSourceBurst, authAttemptKeys),
	}
}

func newAttemptBuckets(perMinute, burst, maxEntries int) *attemptBuckets {
	if perMinute <= 0 || burst <= 0 || maxEntries <= 0 {
		panic("attempt bucket limits must be positive")
	}
	return &attemptBuckets{
		byKey:      make(map[string]*tokenBucket),
		recency:    list.New(),
		perSecond:  float64(perMinute) / 60,
		burst:      float64(burst),
		maxEntries: maxEntries,
	}
}

func (l *authAttemptLimiter) Allow(username, source string, now time.Time) bool {
	// Consume both budgets even when one rejects. Repeated abuse against one
	// account should also exhaust its peer budget, and vice versa.
	accountOK := l.accounts.allow(strings.TrimSpace(username), now)
	sourceOK := l.sources.allow(source, now)
	return accountOK && sourceOK
}

func (b *attemptBuckets) allow(key string, now time.Time) bool {
	if key == "" {
		key = "<empty>"
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	bucket, exists := b.byKey[key]
	if !exists {
		if len(b.byKey) >= b.maxEntries {
			oldest := b.recency.Back()
			delete(b.byKey, oldest.Value.(string))
			b.recency.Remove(oldest)
		}
		bucket = &tokenBucket{tokens: b.burst, last: now}
		bucket.element = b.recency.PushFront(key)
		b.byKey[key] = bucket
	} else {
		b.recency.MoveToFront(bucket.element)
	}
	if now.After(bucket.last) {
		bucket.tokens += now.Sub(bucket.last).Seconds() * b.perSecond
		if bucket.tokens > b.burst {
			bucket.tokens = b.burst
		}
		bucket.last = now
	}
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

// requestSource keys abuse controls to the TCP peer. Forwarded address headers
// are not consulted because a direct client can forge them unless the deployment
// has a separate trusted-proxy boundary.
func requestSource(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	if r.RemoteAddr == "" {
		return "<unknown>"
	}
	return r.RemoteAddr
}
