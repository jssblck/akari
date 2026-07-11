package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/requestbudget"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
	"github.com/jssblck/akari/internal/server/web"
)

func TestPublicAndAuthenticatedAnalyticsShareSnapshotAndRevocationInvalidates(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	worker := parse.NewWorker(st, 1, 0)
	api := New(st, config.Server{
		AnalyticsSnapshotFreshness: time.Hour,
		AnalyticsSnapshotStaleFor:  time.Hour,
		AnalyticsSnapshotLimit:     8,
	}, worker)
	var computes atomic.Int64
	api.analyticsSnapshots.compute = func(context.Context, analyticsSnapshotKey, time.Time) (analyticsPageSnapshot, error) {
		generation := computes.Add(1)
		return analyticsPageSnapshot{analytics: store.Analytics{Sessions: int(generation)}}, nil
	}
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)

	ctx := context.Background()
	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	client := newClient(t)
	if _, err := client.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := client.PostForm(srv.URL+"/account/overview/publish", url.Values{}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	publicPath := web.PublicOverviewPath(owner.Username)
	resp := mustGet(t, http.DefaultClient, srv.URL+publicPath)
	if got := resp.Header.Get("X-Akari-Analytics-Snapshot"); !strings.HasPrefix(got, "state=miss;") {
		resp.Body.Close()
		t.Fatalf("cold public snapshot header = %q, want miss", got)
	}
	resp.Body.Close()

	authedPath := fmt.Sprintf("/overview?range=%s&user=%d", web.DefaultRange, owner.ID)
	resp = mustGet(t, client, srv.URL+authedPath)
	if got := resp.Header.Get("X-Akari-Analytics-Snapshot"); !strings.HasPrefix(got, "state=hit;") {
		resp.Body.Close()
		t.Fatalf("authenticated snapshot header = %q, want hit", got)
	}
	resp.Body.Close()
	if got := computes.Load(); got != 1 {
		t.Fatalf("public then authenticated views ran %d computes, want 1 shared generation", got)
	}

	if _, err := client.PostForm(srv.URL+"/account/overview/unpublish", url.Values{}); err != nil {
		t.Fatalf("unpublish: %v", err)
	}
	resp = mustGet(t, http.DefaultClient, srv.URL+publicPath)
	if resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Fatalf("revoked public overview = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
	if got := computes.Load(); got != 1 {
		t.Fatalf("revoked request consulted snapshot compute, calls = %d", got)
	}

	if _, err := client.PostForm(srv.URL+"/account/overview/publish", url.Values{}); err != nil {
		t.Fatalf("republish: %v", err)
	}
	resp = mustGet(t, http.DefaultClient, srv.URL+publicPath)
	if got := resp.Header.Get("X-Akari-Analytics-Snapshot"); !strings.HasPrefix(got, "state=miss;") {
		resp.Body.Close()
		t.Fatalf("republished snapshot header = %q, want miss after invalidation", got)
	}
	resp.Body.Close()
	if got := computes.Load(); got != 2 {
		t.Fatalf("republished overview ran %d computes, want a new generation", got)
	}
}

func TestPublicAndAuthenticatedProjectShareSnapshot(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	worker := parse.NewWorker(st, 1, 0)
	api := New(st, config.Server{
		AnalyticsSnapshotFreshness: time.Hour,
		AnalyticsSnapshotLimit:     8,
	}, worker)
	var computes atomic.Int64
	api.analyticsSnapshots.compute = func(context.Context, analyticsSnapshotKey, time.Time) (analyticsPageSnapshot, error) {
		computes.Add(1)
		return analyticsPageSnapshot{}, nil
	}
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)

	ctx := context.Background()
	if _, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), ""); err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if err := st.PublishProjectOverview(ctx, projectID); err != nil {
		t.Fatalf("publish project fixture: %v", err)
	}

	publicPath := web.PublicProjectPath(projectID)
	resp := mustGet(t, http.DefaultClient, srv.URL+publicPath)
	if got := resp.Header.Get("X-Akari-Analytics-Snapshot"); !strings.HasPrefix(got, "state=miss;") {
		resp.Body.Close()
		t.Fatalf("cold public project snapshot header = %q, want miss", got)
	}
	resp.Body.Close()

	client := newClient(t)
	if _, err := client.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	resp = mustGet(t, client, srv.URL+fmt.Sprintf("%s?range=%s", web.ProjectPath(projectID), web.DefaultRange))
	if got := resp.Header.Get("X-Akari-Analytics-Snapshot"); !strings.HasPrefix(got, "state=hit;") {
		resp.Body.Close()
		t.Fatalf("authenticated project snapshot header = %q, want hit", got)
	}
	resp.Body.Close()
	if got := computes.Load(); got != 1 {
		t.Fatalf("public then authenticated project ran %d computes, want 1 shared generation", got)
	}
}

func TestAnalyticsBudgetAppliesOnlyToCoalescedRefresh(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	worker := parse.NewWorker(st, 1, 0)
	api := New(st, config.Server{
		RequestBudgetCapacity:      int(requestbudget.MinCapacity),
		RequestBudgetWaitTimeout:   10 * time.Millisecond,
		AnalyticsSnapshotFreshness: time.Hour,
		AnalyticsSnapshotLimit:     8,
	}, worker)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	api.analyticsSnapshots.now = func() time.Time { return now }
	srv := httptest.NewServer(api.Routes())
	t.Cleanup(srv.Close)

	projectID, err := st.UpsertProject(context.Background(), "github.com/ada/notes", "github.com", "ada", "notes", "notes", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if err := st.PublishProjectOverview(context.Background(), projectID); err != nil {
		t.Fatalf("publish project: %v", err)
	}
	path := srv.URL + web.PublicProjectPath(projectID)
	resp := mustGet(t, http.DefaultClient, path)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("X-Akari-Analytics-Snapshot"), "state=miss;") {
		resp.Body.Close()
		t.Fatalf("cold request = (status %d, snapshot %q), want (200, miss)", resp.StatusCode, resp.Header.Get("X-Akari-Analytics-Snapshot"))
	}
	resp.Body.Close()

	hold, err := api.budget.Acquire(context.Background(), requestbudget.MCPSpool)
	if err != nil {
		t.Fatalf("hold budget: %v", err)
	}
	defer hold()
	resp = mustGet(t, http.DefaultClient, path)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("X-Akari-Analytics-Snapshot"), "state=hit;") {
		resp.Body.Close()
		t.Fatalf("warm request under exhausted budget = (status %d, snapshot %q), want (200, hit)", resp.StatusCode, resp.Header.Get("X-Akari-Analytics-Snapshot"))
	}
	resp.Body.Close()

	now = now.Add(2 * time.Hour)
	resp = mustGet(t, http.DefaultClient, path)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expired request under exhausted budget = %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != requestBudgetRetryAfter {
		t.Fatalf("Retry-After = %q, want %q", got, requestBudgetRetryAfter)
	}
}
