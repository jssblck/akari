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

// The projects index is now just the table: no usage panel (the Overview owns
// fleet usage), no page heading (the sidebar marks the section), and no
// local-folder list. The token columns collapse to one "Tokens" total whose
// figure is the sum across all four classes, and the row carries a hover card
// breaking that total back out.
func TestProjectsPageIsBareTable(t *testing.T) {
	p := Page{Title: "Projects", LoggedIn: true, Active: "projects", Username: "Ada Lovelace"}
	projects := []store.ProjectSummary{{
		ID: 1, RemoteKey: "hopper/akari", Kind: "remote", SessionCount: 3,
		TotalInput: 100, TotalOutput: 50, TotalCacheRead: 30, TotalCacheWrite: 20,
	}}
	html := renderComponent(t, ProjectsPage(p, projects, nil))

	for _, gone := range []string{`data-chart`, `data-heatmap`, `<h2>Usage</h2>`, `<h1>Projects</h1>`} {
		if strings.Contains(html, gone) {
			t.Errorf("projects index should be a bare table; found stripped markup %q", gone)
		}
	}
	// The merged column shows the grand total (100+50+30+20 = 200) with thousands
	// separators, plus all four classes in the hover card. Each class is asserted
	// as its full dt/dd pair with a distinct value, so dropping a row or wiring a
	// class to the wrong figure (for example In showing the output total) fails.
	for _, want := range []string{
		`<th class="num">Tokens</th>`, `class="tok-total">200<`,
		`<dt>In</dt><dd>100</dd>`, `<dt>Out</dt><dd>50</dd>`,
		`<dt>Cache read</dt><dd>30</dd>`, `<dt>Cache write</dt><dd>20</dd>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("projects tokens cell missing %q", want)
		}
	}
	if strings.Contains(html, `<th class="num">Input</th>`) {
		t.Error("projects index should fold Input/Output/Cost into one Tokens column, not list Input")
	}
}

// GlobalSessionList drives the redesigned global session table. A non-empty
// render must drop the session-id column, fold state into one Tags column (the
// local-kind chip plus the public visibility chip), show the Tokens cell with
// its breakdown card and cost, and render Updated as relative time carrying the
// exact stamp as a title.
func TestGlobalSessionListColumns(t *testing.T) {
	ts := time.Now().UTC().Add(-3 * 24 * time.Hour)
	rows := []store.SessionRow{{
		SessionSummary: store.SessionSummary{
			ID: 7, Agent: "claude", GitBranch: "main", Username: "grace",
			MessageCount: 12,
			TotalInput:   100, TotalOutput: 50, TotalCacheRead: 7, TotalCacheWrite: 3,
			TotalCostUSD: 1.25, Visibility: "public", UpdatedAt: &ts,
		},
		ProjectID: 4, ProjectKey: "scratch", ProjectName: "scratch", ProjectKind: "standalone",
	}}
	html := renderComponent(t, GlobalSessionList(rows))

	// Tags column carries both the local-kind chip and the public chip.
	for _, want := range []string{`>Tags</th>`, `class="tag standalone"`, `class="tag public"`, `>public</span>`} {
		if !strings.Contains(html, want) {
			t.Errorf("tags column missing %q", want)
		}
	}
	// Tokens cell: the total, the breakdown rows, and the cost, replacing Cost.
	for _, want := range []string{`>Tokens</th>`, "160 tokens", "<dt>In</dt>", "<dt>Out</dt>", "<dt>Cache read</dt>", "<dt>Cache write</dt>", "$1.25"} {
		if !strings.Contains(html, want) {
			t.Errorf("tokens cell missing %q", want)
		}
	}
	if strings.Contains(html, `>Cost</th>`) {
		t.Error("Cost column should be gone")
	}
	// Updated reads relative, with the exact stamp as the cell title.
	if rel := FmtRelTime(&ts); !strings.Contains(html, ">"+rel+"<") {
		t.Errorf("updated cell missing relative time %q", rel)
	}
	if titled := `title="` + FmtTime(&ts) + `"`; !strings.Contains(html, titled) {
		t.Errorf("updated cell missing exact-time title %q", titled)
	}
	// The session-id column is gone: no "#7" label and no Session header.
	if strings.Contains(html, "#7") || strings.Contains(html, `>Session</th>`) {
		t.Error("session-id column should be dropped")
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
