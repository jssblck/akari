package httpapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/requestbudget"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

const validOAuthRegistration = `{"client_name":"Ada's agent","redirect_uris":["http://127.0.0.1/callback"]}`

func newBudgetTestServer(t *testing.T, wait time.Duration) (*Server, *httptest.Server, *store.Store) {
	t.Helper()
	st := storetest.NewStore(t)
	api := New(st, config.Server{
		RequestBudgetCapacity:     int(requestbudget.MinCapacity),
		RequestBudgetWaitTimeout:  wait,
		OAuthRegistrationsPerHour: 1000,
	}, parse.NewWorker(st, 1, 0))
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)
	return api, srv, st
}

func TestExpensiveBurstWaitsAndCompletes(t *testing.T) {
	api, srv, _ := newBudgetTestServer(t, time.Second)
	hold, err := api.budget.Acquire(context.Background(), requestbudget.MCPSpool)
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan *http.Response, 1)
	errs := make(chan error, 1)
	go func() {
		resp, err := http.Post(srv.URL+"/oauth/register", "application/json", strings.NewReader(validOAuthRegistration))
		if err != nil {
			errs <- err
			return
		}
		result <- resp
	}()
	waitForMetric(t, srv.URL+"/metrics", `akari_request_budget_queue_depth{class="oauth_registration"} 1`)
	hold()

	select {
	case err := <-errs:
		t.Fatalf("registration: %v", err)
	case resp := <-result:
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("registration status = %d, want 201: %s", resp.StatusCode, body)
		}
	case <-time.After(time.Second):
		t.Fatal("queued registration did not complete after capacity was released")
	}
}

func TestAdmissionTimeoutIsRetryable(t *testing.T) {
	api, srv, _ := newBudgetTestServer(t, 10*time.Millisecond)
	hold, err := api.budget.Acquire(context.Background(), requestbudget.MCPSpool)
	if err != nil {
		t.Fatal(err)
	}
	defer hold()

	resp, err := http.Post(srv.URL+"/oauth/register", "application/json", strings.NewReader(validOAuthRegistration))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != requestBudgetRetryAfter {
		t.Fatalf("Retry-After = %q, want %q", got, requestBudgetRetryAfter)
	}
}

func TestOAuthRegistrationGrowthCeilingIsRetryable(t *testing.T) {
	api, srv, _ := newBudgetTestServer(t, time.Second)
	api.Cfg.OAuthRegistrationsPerHour = 1
	for attempt := 1; attempt <= 2; attempt++ {
		resp, err := http.Post(srv.URL+"/oauth/register", "application/json", strings.NewReader(validOAuthRegistration))
		if err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		resp.Body.Close()
		if attempt == 1 && resp.StatusCode != http.StatusCreated {
			t.Fatalf("first registration status = %d, want 201", resp.StatusCode)
		}
		if attempt == 2 {
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("second registration status = %d, want 429", resp.StatusCode)
			}
			if got := resp.Header.Get("Retry-After"); got != "3600" {
				t.Fatalf("Retry-After = %q, want 3600", got)
			}
		}
	}
}

func TestExpensiveRoutesPublishClassMetrics(t *testing.T) {
	_, srv, _ := newBudgetTestServer(t, time.Second)
	requests := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/auth/register", `{"username":"grace","password":"hopper-1906"}`},
		{http.MethodPost, "/oauth/register", validOAuthRegistration},
		{http.MethodPost, "/mcp", `{}`},
		{http.MethodGet, "/p/999999", ""},
	}
	for _, req := range requests {
		r, err := http.NewRequest(req.method, srv.URL+req.path, strings.NewReader(req.body))
		if err != nil {
			t.Fatal(err)
		}
		if req.body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatalf("%s %s: %v", req.method, req.path, err)
		}
		resp.Body.Close()
	}

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range []string{"password", "oauth_registration", "mcp_spool", "public_analytics"} {
		want := `akari_request_budget_acquired_total{class="` + class + `"} 1`
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics missing %q\n%s", want, body)
		}
	}
}

// TestOGCacheHitConsumesNoBudget guards the core of the fix: a warm cache never
// reaches admission, so it must keep serving even when the whole process budget is
// held elsewhere. It holds every unit of a MinCapacity budget for the test's
// duration (weight 12, matching the configured capacity exactly, so even the
// lightest class could not squeeze in), seeds a published overview with a cached
// card, and confirms /u/<username>/og.png still serves 200 with the cached bytes. If
// the removed route-level s.admit wrapper (or an equivalent gate before the TTL
// check) were reintroduced, this request would instead wait out the budget and 503.
func TestOGCacheHitConsumesNoBudget(t *testing.T) {
	api, srv, st := newBudgetTestServer(t, 10*time.Millisecond)
	ctx := context.Background()

	hold, err := api.budget.Acquire(ctx, requestbudget.MCPSpool)
	if err != nil {
		t.Fatalf("hold full capacity: %v", err)
	}
	defer hold()

	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.PublishOverview(ctx, owner.ID); err != nil {
		t.Fatalf("publish overview: %v", err)
	}
	sentinel := []byte("cached-sentinel-not-a-real-png")
	if _, err := st.PutOverviewOGImage(ctx, owner.ID, sentinel, time.Now()); err != nil {
		t.Fatalf("seed cached card: %v", err)
	}

	resp, err := http.Get(srv.URL + "/u/grace/og.png")
	if err != nil {
		t.Fatalf("get og.png: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cache hit under an exhausted budget = %d, want 200 (cache hits must not admit)", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !bytes.Equal(body, sentinel) {
		t.Fatalf("cache hit served %d bytes, want the cached sentinel unchanged", len(body))
	}
}

// TestOGColdMissWithoutFallbackIsRetryableWhenBudgetExhausted confirms admission
// moving inside the coalesced render (see renderCoalesced) preserves the
// route-level wrapper's old failure mode: a cold render (no cached card to fall
// back to) that cannot get admitted within the wait bound answers the same
// retryable 503-with-Retry-After a whole-handler s.admit wrapper used to produce,
// rather than hanging or surfacing as an opaque 500.
func TestOGColdMissWithoutFallbackIsRetryableWhenBudgetExhausted(t *testing.T) {
	api, srv, st := newBudgetTestServer(t, 10*time.Millisecond)
	ctx := context.Background()

	hold, err := api.budget.Acquire(ctx, requestbudget.MCPSpool)
	if err != nil {
		t.Fatalf("hold full capacity: %v", err)
	}
	defer hold()

	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.PublishOverview(ctx, owner.ID); err != nil {
		t.Fatalf("publish overview: %v", err)
	}
	// No cached card is seeded: the cache is cold, so a render is required and there
	// is nothing to fall back to when it cannot be admitted.

	resp, err := http.Get(srv.URL + "/u/grace/og.png")
	if err != nil {
		t.Fatalf("get og.png: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("cold miss under an exhausted budget = %d, want 503: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Retry-After"); got != requestBudgetRetryAfter {
		t.Fatalf("Retry-After = %q, want %q", got, requestBudgetRetryAfter)
	}
}

// TestOGRenderCoalescesAdmissionAcrossConcurrentMisses guards the other half of the
// fix: N concurrent misses for the same entity must charge exactly one admission,
// not one per waiter. renderCoalesced is the shared machinery behind all three OG
// render paths, so this exercises it directly with a synthetic render held open by a
// channel (mirroring TestInsightsRefresherColdGetsCoalesce's technique for the
// unrelated insights singleflight): every goroutine calls it with the same key
// before the render is allowed to finish, so if admission happened around each
// caller rather than inside the singleflight leader's closure, either a fair-FIFO
// semaphore would starve most of six weight-4 requests against a 12-unit budget, or
// (if it did admit all six) the acquired counter would read 6, not 1.
func TestOGRenderCoalescesAdmissionAcrossConcurrentMisses(t *testing.T) {
	api, srv, _ := newBudgetTestServer(t, time.Second)

	const waiters = 6
	var renders atomic.Int64
	release := make(chan struct{})
	sf := &singleflight.Group{}
	want := []byte("rendered-card")

	var wg sync.WaitGroup
	results := make(chan []byte, waiters)
	errs := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := api.renderCoalesced(context.Background(), sf, "entity-1", requestbudget.PublicAnalytics,
				func(context.Context) ([]byte, error) {
					renders.Add(1)
					<-release // hold the render open so every waiter queues behind it
					return want, nil
				})
			if err != nil {
				errs <- err
				return
			}
			results <- got
		}()
	}
	// Give every goroutine time to converge on the singleflight key before releasing.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Errorf("waiter returned an error: %v", err)
	}
	if got := renders.Load(); got != 1 {
		t.Fatalf("expected %d concurrent misses to coalesce into 1 render, got %d", waiters, got)
	}
	gotResults := 0
	for got := range results {
		gotResults++
		if !bytes.Equal(got, want) {
			t.Errorf("waiter got %q, want %q", got, want)
		}
	}
	if gotResults != waiters {
		t.Fatalf("got %d successful waiters, want %d", gotResults, waiters)
	}

	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if want := `akari_request_budget_acquired_total{class="public_analytics"} 1`; !strings.Contains(string(body), want) {
		t.Fatalf("metrics missing %q (want exactly one admission for the coalesced render)\n%s", want, body)
	}
}

func waitForMetric(t *testing.T, url, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatalf("metric %q did not appear: %v", want, ctx.Err())
			}
			continue
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(body), want) {
			return
		}
		if ctx.Err() != nil {
			t.Fatalf("metric %q did not appear", want)
		}
	}
}
