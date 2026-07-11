package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/requestbudget"
	"github.com/jssblck/akari/internal/server/storetest"
)

const validOAuthRegistration = `{"client_name":"Ada's agent","redirect_uris":["http://127.0.0.1/callback"]}`

func newBudgetTestServer(t *testing.T, wait time.Duration) (*Server, *httptest.Server) {
	t.Helper()
	st := storetest.NewStore(t)
	api := New(st, config.Server{
		RequestBudgetCapacity:     int(requestbudget.MinCapacity),
		RequestBudgetWaitTimeout:  wait,
		OAuthRegistrationsPerHour: 1000,
	}, parse.NewWorker(st, 1, 0))
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)
	return api, srv
}

func TestExpensiveBurstWaitsAndCompletes(t *testing.T) {
	api, srv := newBudgetTestServer(t, time.Second)
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
	api, srv := newBudgetTestServer(t, 10*time.Millisecond)
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

func TestUnpublishedAnalyticsChecksAccessBeforeAdmission(t *testing.T) {
	api, srv := newBudgetTestServer(t, 10*time.Millisecond)
	hold, err := api.budget.Acquire(context.Background(), requestbudget.MCPSpool)
	if err != nil {
		t.Fatal(err)
	}
	defer hold()

	resp, err := http.Get(srv.URL + "/p/999999")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unpublished analytics under exhausted budget = %d, want gate-first 404", resp.StatusCode)
	}
}

func TestOAuthRegistrationGrowthCeilingIsRetryable(t *testing.T) {
	api, srv := newBudgetTestServer(t, time.Second)
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
	api, srv := newBudgetTestServer(t, time.Second)
	projectID, err := api.Store.UpsertProject(context.Background(), "github.com/ada/notes", "github.com", "ada", "notes", "notes", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if err := api.Store.PublishProjectOverview(context.Background(), projectID); err != nil {
		t.Fatalf("publish project: %v", err)
	}
	requests := []struct {
		method string
		path   string
		body   string
	}{
		{http.MethodPost, "/api/v1/auth/register", `{"username":"grace","password":"hopper-1906"}`},
		{http.MethodPost, "/oauth/register", validOAuthRegistration},
		{http.MethodPost, "/mcp", `{}`},
		{http.MethodGet, fmt.Sprintf("/p/%d", projectID), ""},
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
