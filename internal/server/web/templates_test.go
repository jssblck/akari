package web

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// renderComponent renders a templ component to a string for markup assertions.
func renderComponent(t *testing.T, c interface {
	Render(context.Context, io.Writer) error
}) string {
	t.Helper()
	var b strings.Builder
	if err := c.Render(context.Background(), &b); err != nil {
		t.Fatalf("render: %v", err)
	}
	return b.String()
}

// analyticsWithData returns an Analytics that reports HasData, so the panel
// renders its chart (or heatmap) branch rather than the empty state.
func analyticsWithData() store.Analytics {
	return store.Analytics{
		Sessions:  3,
		TotalCost: 12.5,
		TotalIn:   100,
		TotalOut:  50,
		Series: []store.DayPoint{{
			Day:   time.Date(2026, 6, 3, 0, 0, 0, 0, time.UTC),
			Input: 100, Output: 20, CacheRead: 5, CacheWrite: 2, CostUSD: 1.25,
		}},
	}
}

// The overview is the only page that renders usage as a calendar heatmap. It
// must emit the heatmap container and its Tokens/Dollars control, must not fall
// back to the line-chart markup, and must drop the "Usage" panel header that
// would otherwise repeat the scope already named in the page head.
func TestOverviewPageRendersHeatmap(t *testing.T) {
	p := Page{Title: "Overview", LoggedIn: true, Active: "overview", Username: "Grace Hopper"}
	html := renderComponent(t, OverviewPage(p, analyticsWithData(), nil))

	for _, want := range []string{`data-heatmap`, `data-heatmap-target="chart-global"`, `>Tokens</button>`, `>Dollars</button>`} {
		if !strings.Contains(html, want) {
			t.Errorf("overview should render the heatmap; missing %q", want)
		}
	}
	if strings.Contains(html, `data-chart`) {
		t.Error("overview should not render the line-chart markup")
	}
	if strings.Contains(html, `<h2>Usage</h2>`) {
		t.Error("overview should drop the redundant Usage panel header")
	}
}

// The projects index keeps the line chart and the titled Usage panel: the page
// head says "Projects", so "Usage / Across all projects" is not a repeat.
func TestProjectsPageKeepsLineChart(t *testing.T) {
	p := Page{Title: "Projects", LoggedIn: true, Active: "projects", Username: "Ada Lovelace"}
	projects := []store.ProjectSummary{{ID: 1, RemoteKey: "hopper/akari", Kind: "remote", SessionCount: 3}}
	html := renderComponent(t, ProjectsPage(p, projects, nil, analyticsWithData(), nil))

	for _, want := range []string{`data-chart`, `data-chart-target="chart-global"`, `<h2>Usage</h2>`} {
		if !strings.Contains(html, want) {
			t.Errorf("projects index should render the line chart; missing %q", want)
		}
	}
	if strings.Contains(html, `data-heatmap`) {
		t.Error("projects index should not render the heatmap")
	}
}

// A single project page also keeps the line chart, keyed to its own chart id and
// retaining the Usage panel header.
func TestProjectPageKeepsLineChart(t *testing.T) {
	p := Page{Title: "akari", LoggedIn: true, Active: "projects", Username: "Anna Winlock"}
	proj := store.ProjectSummary{ID: 7, RemoteKey: "hopper/akari", Kind: "remote", SessionCount: 1}
	html := renderComponent(t, ProjectPage(p, proj, nil, Facets{}, store.SessionFilter{}, analyticsWithData()))

	for _, want := range []string{`data-chart`, `data-chart-target="chart-project"`, `<h2>Usage</h2>`} {
		if !strings.Contains(html, want) {
			t.Errorf("project page should render the line chart; missing %q", want)
		}
	}
	if strings.Contains(html, `data-heatmap`) {
		t.Error("project page should not render the heatmap")
	}
}
