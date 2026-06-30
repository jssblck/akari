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
		Sessions:        3,
		TotalCost:       12.5,
		TotalIn:         100,
		TotalOut:        50,
		TotalCacheRead:  30,
		TotalCacheWrite: 12,
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
	html := renderComponent(t, OverviewPage(p, analyticsWithData(), DefaultRange, nil, nil))

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

// The overview's usage panel is the htmx swap target for the range selector, so
// it must carry the stable id and the selector must offer every window, mark the
// active one, and refetch into that same target.
func TestOverviewPageRangeSelector(t *testing.T) {
	p := Page{Title: "Overview", LoggedIn: true, Active: "overview", Username: "Grace Hopper"}
	html := renderComponent(t, OverviewPage(p, analyticsWithData(), "90d", nil, nil))

	for _, want := range []string{`id="usage"`, `aria-label="Date range"`, `hx-get="/?range=7d"`, `hx-get="/?range=all"`, `hx-target="#usage"`, `hx-select="#usage"`} {
		if !strings.Contains(html, want) {
			t.Errorf("range selector missing %q", want)
		}
	}
	for _, dr := range DateRanges {
		if !strings.Contains(html, ">"+dr.Label+"</button>") {
			t.Errorf("range selector should offer %q", dr.Label)
		}
	}
	// The active window (90d) is the one marked.
	if !strings.Contains(html, `class="seg active" hx-get="/?range=90d"`) {
		t.Error("the active range button should carry the active class")
	}
}

// The per-user filter sits beside the range selector: a disclosure offering an
// "All Users" reset and a checkbox per account, the active selection marked both
// as checked boxes and as collapsed pills. The range buttons must carry the active
// users so switching the window holds the selection, and the menu must serialize
// its hidden range plus the checked boxes back to the overview.
func TestOverviewPageUserFilter(t *testing.T) {
	p := Page{Title: "Overview", LoggedIn: true, Active: "overview", Username: "Grace Hopper"}
	users := []store.User{{ID: 2, Username: "ada"}, {ID: 5, Username: "grace"}}
	html := renderComponent(t, OverviewPage(p, analyticsWithData(), "7d", users, []int64{5}))

	for _, want := range []string{
		`class="userfilter"`,
		`class="userfilter-opt userfilter-reset"`,
		`hx-get="/?range=7d"`, // the reset clears users while holding the window
		`name="user" value="2"`,
		`name="user" value="5"`,
		`hx-include="closest .userfilter-menu"`,
		`<input type="hidden" name="range" value="7d">`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("user filter missing %q", want)
		}
	}
	// The selected account renders a collapsed pill and a checked box.
	if !strings.Contains(html, `class="userfilter-pill">grace</span>`) {
		t.Error("the selected user should show as a collapsed pill")
	}
	if !strings.Contains(html, `value="5" checked`) {
		t.Error("the selected user's checkbox should be checked")
	}
	// The range buttons preserve the selection, so switching the window keeps it.
	// templ HTML-escapes the & in the attribute value (htmx decodes it back).
	if !strings.Contains(html, `hx-get="/?range=30d&amp;user=5"`) {
		t.Error("range buttons should carry the active user selection")
	}
}

// With nothing selected the collapsed control reads "All Users" and the reset is
// the active option, so the unscoped state is legible without opening the menu.
func TestOverviewPageUserFilterUnscoped(t *testing.T) {
	p := Page{Title: "Overview", LoggedIn: true, Active: "overview", Username: "Grace Hopper"}
	users := []store.User{{ID: 2, Username: "ada"}}
	html := renderComponent(t, OverviewPage(p, analyticsWithData(), DefaultRange, users, nil))

	if !strings.Contains(html, `class="userfilter-all">All Users</span>`) {
		t.Error("unscoped collapsed control should read All Users")
	}
	// The All Users reset's decorative box renders checked when nothing is selected.
	if !strings.Contains(html, `aria-hidden="true" checked>`) {
		t.Error("unscoped state should mark the All Users reset box checked")
	}
}

// The overview's headline strip reads Cost / Tokens / Sessions, with the combined
// token figure and its per-class split (the same in/out/cache breakdown a heatmap
// cell shows) behind the Tokens tooltip. It shares the session header's Tokens tile
// classes (tokens-stat trigger, stat-tip popup) and no longer splits Input and
// Output into their own tiles.
func TestOverviewPageTokensStat(t *testing.T) {
	p := Page{Title: "Overview", LoggedIn: true, Active: "overview", Username: "Grace Hopper"}
	html := renderComponent(t, OverviewPage(p, analyticsWithData(), DefaultRange, nil, nil))

	for _, want := range []string{`>Tokens</div>`, `tokens-stat`, `tokens-value`, `class="stat-tip"`, `<dt>In</dt>`, `<dt>Out</dt>`, `<dt>Cache read</dt>`, `<dt>Cache write</dt>`} {
		if !strings.Contains(html, want) {
			t.Errorf("tokens readout missing %q", want)
		}
	}
	// Combined total: 100 + 50 + 30 + 12 = 192.
	if !strings.Contains(html, `>192</div>`) {
		t.Error("tokens readout should show the combined token total (192)")
	}
	if strings.Contains(html, `>Input</div>`) || strings.Contains(html, `>Output</div>`) {
		t.Error("the Input/Output tiles should be folded into the Tokens readout")
	}
}

// The landing surface drops the recent-activity feed and the redundant scope
// subtitle: the panel is the page.
func TestOverviewPageDropsRecentActivity(t *testing.T) {
	p := Page{Title: "Overview", LoggedIn: true, Active: "overview", Username: "Grace Hopper"}
	html := renderComponent(t, OverviewPage(p, analyticsWithData(), DefaultRange, nil, nil))

	if strings.Contains(html, "Recent activity") {
		t.Error("overview should no longer render the recent-activity feed")
	}
	if strings.Contains(html, "across all projects") {
		t.Error("overview should drop the scope subtitle")
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

// GlobalSessionList renders the cross-project feed. A row carries the session's
// metadata: the project leads as the anchor, the branch the detail, with the agent
// and state chips alongside, the token figure with its breakdown card and cost, and
// the whole row links to the session. No prompt content and no session id are
// printed.
func TestGlobalSessionListRow(t *testing.T) {
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
	html := renderComponent(t, GlobalSessionList(rows, store.SessionFilter{Sort: "updated", Desc: true}))

	for _, want := range []string{
		// The whole row links to the session.
		`data-row-href="/sessions/7"`,
		// The row carries the agent, project, branch, and both state chips.
		`>claude</span>`, `>scratch</span>`, `>main</span>`,
		`class="tag standalone"`, `class="tag public"`,
		// Tokens: compact figure plus the breakdown card and cost.
		`160 tokens`, "<dt>In</dt>", "<dt>Out</dt>", "<dt>Cache read</dt>", "<dt>Cache write</dt>", "$1.25",
		// Time reads as the clock with the exact stamp on hover.
		`title="` + FmtTime(&ts) + `"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("feed row missing %q", want)
		}
	}
	// The session id is never printed, and the old table chrome is gone.
	if strings.Contains(html, "#7") || strings.Contains(html, "<table") || strings.Contains(html, "sort-link") {
		t.Error("feed should drop the session id, the table, and the sort headers")
	}
}

// In the default most-recent order the feed groups rows under day headings, and a
// row whose project repeats the row above it fades; any other sort renders a flat,
// ungrouped list.
func TestGlobalSessionListGrouping(t *testing.T) {
	// Anchor both rows at midday today so they share a UTC calendar day (and the
	// Today heading) no matter when the test runs.
	n := time.Now().UTC()
	today := time.Date(n.Year(), n.Month(), n.Day(), 12, 0, 0, 0, time.UTC)
	earlier := today.Add(-2 * time.Hour)
	rows := []store.SessionRow{
		{SessionSummary: store.SessionSummary{ID: 1, Agent: "claude", UpdatedAt: &today}, ProjectID: 1, ProjectKey: "akari", ProjectName: "akari", ProjectKind: "remote"},
		{SessionSummary: store.SessionSummary{ID: 2, Agent: "claude", UpdatedAt: &earlier}, ProjectID: 1, ProjectKey: "akari", ProjectName: "akari", ProjectKind: "remote"},
	}

	grouped := renderComponent(t, GlobalSessionList(rows, store.SessionFilter{Sort: "updated", Desc: true}))
	if !strings.Contains(grouped, `class="day-head"`) || !strings.Contains(grouped, `>Today</span>`) {
		t.Error("most-recent order should render a day heading")
	}
	// The second row repeats the first row's project, so its label fades.
	if !strings.Contains(grouped, `class="srow-proj faded"`) {
		t.Error("a repeated project label should fade")
	}

	flat := renderComponent(t, GlobalSessionList(rows, store.SessionFilter{Sort: "tokens", Desc: true}))
	if strings.Contains(flat, `class="day-head"`) {
		t.Error("a non-recent sort should not day-group the feed")
	}
}

// The project page now reads exactly like the overview, scoped to one project: it
// renders the calendar heatmap (keyed to its own chart id) with the Tokens/Dollars
// toggle and a window selector that refetches the panel from the project's own
// path, preserving any active session filter. It carries no line chart and no
// redundant "Usage" panel header (the page head already names the project).
func TestProjectPageRendersHeatmap(t *testing.T) {
	p := Page{Title: "akari", LoggedIn: true, Active: "projects", Username: "Anna Winlock"}
	proj := store.ProjectSummary{ID: 7, RemoteKey: "hopper/akari", Kind: "remote", SessionCount: 1}
	sel := store.SessionFilter{ProjectID: 7, Agent: "claude"}
	html := renderComponent(t, ProjectPage(p, proj, nil, Facets{}, sel, analyticsWithData(), "90d"))

	for _, want := range []string{
		`data-heatmap`, `data-heatmap-target="chart-project"`, `>Tokens</button>`, `>Dollars</button>`,
		`id="usage"`, `aria-label="Date range"`,
		// Panel and table share one swappable region so a range or filter change
		// refetches both and they reflect the same scope.
		`id="project-view"`,
		// The selector refetches the project's own path, carries the active window,
		// and rides the active agent filter so switching the window keeps it.
		`hx-get="/projects/7?agent=claude&amp;range=7d"`,
		`class="seg active" hx-get="/projects/7?agent=claude&amp;range=90d"`,
		`hx-target="#project-view"`, `hx-select="#project-view"`,
		// The filter form carries the window so a filter submit does not reset it.
		`<input type="hidden" name="range" value="90d"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("project page should read like the overview; missing %q", want)
		}
	}
	if strings.Contains(html, `data-chart-target`) {
		t.Error("project page should not render the line chart")
	}
	if strings.Contains(html, `<h2>Usage</h2>`) {
		t.Error("project page should drop the redundant Usage panel header")
	}
	// The totals strip and activity grid sit in the centered lead column so the
	// calendar grid reads at the Overview's width on this full-bleed page; the
	// breakdowns stay a full-width sibling after the lead closes.
	lead := strings.Index(html, `class="usage-lead"`)
	grid := strings.Index(html, `data-heatmap`)
	breakdowns := strings.Index(html, `class="breakdowns"`)
	if lead < 0 || !(lead < grid && grid < breakdowns) {
		t.Errorf("activity grid should sit inside the centered usage-lead, breakdowns after it; got lead=%d grid=%d breakdowns=%d", lead, grid, breakdowns)
	}
}
