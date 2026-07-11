package requestbudget

import (
	"context"
	"errors"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBudgetBoundsMixedConcurrentWork(t *testing.T) {
	b, err := New(DefaultCapacity, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	classes := []WorkClass{PublicAnalytics, PublicAnalytics, OAuthRegistration, OAuthRegistration}

	start := make(chan struct{})
	finish := make(chan struct{})
	acquired := make(chan struct{}, len(classes))
	var wg sync.WaitGroup
	var mu sync.Mutex
	current, peak := int64(0), int64(0)
	for _, class := range classes {
		class := class
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, err := b.Acquire(context.Background(), class)
			if err != nil {
				name, _, _ := class.spec()
				t.Errorf("Acquire(%s): %v", name, err)
				return
			}
			mu.Lock()
			_, weight, _ := class.spec()
			current += weight
			if current > peak {
				peak = current
			}
			if current > DefaultCapacity {
				t.Errorf("in-flight weight = %d, capacity = %d", current, DefaultCapacity)
			}
			mu.Unlock()
			acquired <- struct{}{}
			<-finish
			mu.Lock()
			current -= weight
			mu.Unlock()
			release()
		}()
	}
	close(start)
	for range classes {
		<-acquired
	}
	close(finish)
	wg.Wait()
	if peak != 10 {
		t.Fatalf("peak weight = %d, want 10 from the admitted mixed load", peak)
	}
}

func TestBudgetLoadBoundsWorkClassConcurrency(t *testing.T) {
	for _, class := range []WorkClass{MCPSpool, PublicAnalytics, OAuthRegistration} {
		class := class
		name, weight, _ := class.spec()
		t.Run(name, func(t *testing.T) {
			b, err := New(DefaultCapacity, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			limit := int(DefaultCapacity / weight)
			workers := limit + 8
			start := make(chan struct{})
			finish := make(chan struct{})
			entered := make(chan struct{}, workers)
			var wg sync.WaitGroup
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					release, err := b.Acquire(context.Background(), class)
					if err != nil {
						t.Errorf("Acquire: %v", err)
						return
					}
					entered <- struct{}{}
					<-finish
					release()
				}()
			}
			close(start)
			for i := 0; i < limit; i++ {
				<-entered
			}
			select {
			case <-entered:
				t.Fatalf("more than %d %s requests entered at capacity %d", limit, name, DefaultCapacity)
			default:
			}
			close(finish)
			wg.Wait()
		})
	}
}

func TestBudgetWaitsThenAdmitsBurst(t *testing.T) {
	b, err := New(MinCapacity, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	first, err := b.Acquire(context.Background(), MCPSpool)
	if err != nil {
		t.Fatal(err)
	}

	admitted := make(chan error, 1)
	go func() {
		release, err := b.Acquire(context.Background(), OAuthRegistration)
		if err == nil {
			release()
		}
		admitted <- err
	}()
	name, _, _ := OAuthRegistration.spec()
	waitForQueueDepth(t, b, name, 1)
	select {
	case err := <-admitted:
		t.Fatalf("queued work returned before capacity was released: %v", err)
	default:
	}
	first()
	if err := <-admitted; err != nil {
		t.Fatalf("queued work was not admitted: %v", err)
	}
}

func waitForQueueDepth(t *testing.T, b *Budget, class string, want int64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		b.mu.Lock()
		got := b.metrics[class].queued
		b.mu.Unlock()
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue depth for %s = %d, want %d", class, got, want)
		}
		runtime.Gosched()
	}
}

func TestBudgetTimeoutAndCancellationDoNotLeakCapacity(t *testing.T) {
	b, err := New(MinCapacity, 20*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	hold, err := b.Acquire(context.Background(), MCPSpool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Acquire(context.Background(), OAuthRegistration); !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("timed out Acquire = %v, want ErrWaitTimeout", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Acquire(ctx, OAuthRegistration); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Acquire = %v, want context.Canceled", err)
	}
	hold()

	release, err := b.Acquire(context.Background(), MCPSpool)
	if err != nil {
		t.Fatalf("capacity leaked after timeout or cancellation: %v", err)
	}
	release()
	release()
}

func TestBudgetMetricsExposeQueueWaitRejectionAndUtilization(t *testing.T) {
	b, err := New(MinCapacity, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	hold, err := b.Acquire(context.Background(), MCPSpool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Acquire(context.Background(), PublicAnalytics); !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("Acquire = %v, want timeout", err)
	}

	rr := httptest.NewRecorder()
	b.ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()
	for _, want := range []string{
		`akari_request_budget_queue_depth{class="public_analytics"} 0`,
		`akari_request_budget_in_use_weight{class="mcp_spool"} 12`,
		`akari_request_budget_utilization_ratio{class="mcp_spool"} 1`,
		`akari_request_budget_rejected_total{class="public_analytics",reason="timeout"} 1`,
		`akari_request_budget_wait_seconds_count{class="public_analytics"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q\n%s", want, body)
		}
	}
	hold()
}

func TestNewRejectsBudgetThatCannotRunEveryClass(t *testing.T) {
	if _, err := New(MinCapacity-1, time.Second); err == nil {
		t.Fatal("New accepted capacity below the heaviest work class")
	}
	if _, err := New(DefaultCapacity, 0); err == nil {
		t.Fatal("New accepted an unbounded zero wait timeout")
	}
	b, err := New(DefaultCapacity, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Acquire(context.Background(), WorkClass(255)); !errors.Is(err, ErrInvalidClass) {
		t.Fatalf("Acquire with unknown work class = %v, want ErrInvalidClass", err)
	}
}
