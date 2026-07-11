package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
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

// totalInFigure extracts the rendered "In" token total from a page's Tokens tile
// (statTokens in overview.templ, shared by both OverviewPage and ProjectPage), so a
// test can compare the same figure across two requests without asserting on the
// surrounding markup.
var totalInFigure = regexp.MustCompile(`<dt>In</dt>\s*<dd>([^<]+)</dd>`)

func mustTotalIn(t *testing.T, body string) string {
	t.Helper()
	m := totalInFigure.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("could not find the token tile's In total in the page body:\n%s", body)
	}
	return m[1]
}

// seedPastAndFutureUsage announces one session for owner in projectID and records two
// usage events on it: one dated yesterday (counted by every window) and one dated three
// days from now (past ogimage.DefaultUntil's end-of-today bound, so a correctly bounded
// read excludes it). Returns the input-token figure a correctly bounded read must show:
// the past event's 100, never the combined 1000.
func seedPastAndFutureUsage(t *testing.T, st *store.Store, ownerID, projectID int64) {
	t.Helper()
	ctx := context.Background()
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: ownerID, Agent: "claude", SourceSessionID: "sess-past-future",
		ProjectID: projectID, Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	stampSessionCurrent(t, st, ann.SessionID)
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1, 'claude-opus-4-8', 100, 50, 1.0, now() - make_interval(days => 1), 'past')`,
		ann.SessionID); err != nil {
		t.Fatalf("seed past usage: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO usage_events (session_id, model, input_tokens, output_tokens, cost_usd, occurred_at, dedup_key)
		 VALUES ($1, 'claude-opus-4-8', 900, 90, 9.0, now() + make_interval(days => 3), 'future')`,
		ann.SessionID); err != nil {
		t.Fatalf("seed future usage: %v", err)
	}
}

// TestOverviewFutureUsageBoundedIdenticallyOnBothPaths guards the review fix directly:
// handleOverview's shared-snapshot path (exactly one user selected) and its live-read
// path (the else branch, taken here by the bare unfiltered load) must apply the same
// Until bound, or the same overview disagrees with itself depending on whether a
// ?user= filter happens to be present. Before the fix the live path read
// store.AnalyticsFilter with no Until, so it folded the future-dated event into the
// total while the snapshot path excluded it.
func TestOverviewFutureUsageBoundedIdenticallyOnBothPaths(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()
	c := newClient(t)

	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	seedPastAndFutureUsage(t, st, owner.ID, projectID)

	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}

	// Exactly one user selected: the shared-snapshot path (computeAnalyticsSnapshot
	// applies ogimage.DefaultUntil).
	snapshotBody := readBody(t, mustGet(t, c, srv.URL+"/overview?user="+strconv.FormatInt(owner.ID, 10)))
	snapshotTotal := mustTotalIn(t, snapshotBody)

	// No user filter: with a single registered account this reads the same underlying
	// data through the live path (nil UserIDs, unfiltered), which is the branch the
	// review found reading with no Until bound.
	liveBody := readBody(t, mustGet(t, c, srv.URL+"/overview"))
	liveTotal := mustTotalIn(t, liveBody)

	if liveTotal != snapshotTotal {
		t.Fatalf("live-path total In = %s, snapshot-path total In = %s; want equal (both must exclude the future-dated event)", liveTotal, snapshotTotal)
	}
	if snapshotTotal != "100" {
		t.Fatalf("total In = %s, want 100 (the future-dated event must be excluded from both paths)", snapshotTotal)
	}
}

// TestProjectPageFutureUsageBoundedIdenticallyOnBothPaths is the project-page mirror
// of TestOverviewFutureUsageBoundedIdenticallyOnBothPaths: handleProjectPage's
// shared-snapshot path (an unfiltered load) and its live-read path (any agent, user,
// or machine filter) must exclude a future-dated usage event the same way.
func TestProjectPageFutureUsageBoundedIdenticallyOnBothPaths(t *testing.T) {
	t.Parallel()
	srv, st := newTestServer(t)
	ctx := context.Background()
	c := newClient(t)

	owner, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	seedPastAndFutureUsage(t, st, owner.ID, projectID)

	if _, err := c.PostForm(srv.URL+"/login", url.Values{
		"username": {"grace"}, "password": {"hopper-1906"},
	}); err != nil {
		t.Fatalf("login: %v", err)
	}

	base := fmt.Sprintf("/projects/%d", projectID)

	// Unfiltered: the shared-snapshot path.
	snapshotBody := readBody(t, mustGet(t, c, srv.URL+base))
	snapshotTotal := mustTotalIn(t, snapshotBody)

	// Filtered by the agent every seeded event actually used, so the filter narrows
	// nothing real: the live-read path with the same underlying rows as the snapshot
	// path above, which the review found reading with no Until bound.
	liveBody := readBody(t, mustGet(t, c, srv.URL+base+"?agent=claude"))
	liveTotal := mustTotalIn(t, liveBody)

	if liveTotal != snapshotTotal {
		t.Fatalf("live-path total In = %s, snapshot-path total In = %s; want equal (both must exclude the future-dated event)", liveTotal, snapshotTotal)
	}
	if snapshotTotal != "100" {
		t.Fatalf("total In = %s, want 100 (the future-dated event must be excluded from both paths)", snapshotTotal)
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
