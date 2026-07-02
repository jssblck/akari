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
		TotalReasoning:  700,
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

	for _, want := range []string{`id="usage"`, `aria-label="Date range"`, `hx-get="/overview?range=7d"`, `hx-get="/overview?range=all"`, `hx-target="#usage"`, `hx-select="#usage"`} {
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
	if !strings.Contains(html, `class="seg active" hx-get="/overview?range=90d"`) {
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
		`hx-get="/overview?range=7d"`, // the reset clears users while holding the window
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
	if !strings.Contains(html, `hx-get="/overview?range=30d&amp;user=5"`) {
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

	for _, want := range []string{`>Tokens</div>`, `tokens-stat`, `tokens-value`, `class="stat-tip"`, `<dt>In</dt>`, `<dt>Out</dt>`, `<dt>Cache read</dt>`, `<dt>Cache write</dt>`, `<dt>Reasoning</dt>`} {
		if !strings.Contains(html, want) {
			t.Errorf("tokens readout missing %q", want)
		}
	}
	// Combined total: 100 + 50 + 30 + 12 = 192; reasoning (700) stays out of the headline
	// and shows only as its own tooltip line.
	if !strings.Contains(html, `>192</div>`) {
		t.Error("tokens readout should show the combined token total (192)")
	}
	if !strings.Contains(html, `<dd>700</dd>`) {
		t.Error("tokens tooltip should show the reasoning class (700) as its own line")
	}
	if strings.Contains(html, `>Input</div>`) || strings.Contains(html, `>Output</div>`) {
		t.Error("the Input/Output tiles should be folded into the Tokens readout")
	}
}

// A window with no reasoning tokens (a Claude-only fleet) shows no Reasoning line rather
// than a "Reasoning 0" that would read as a real, always-present class.
func TestOverviewPageHidesZeroReasoning(t *testing.T) {
	p := Page{Title: "Overview", LoggedIn: true, Active: "overview", Username: "Grace Hopper"}
	a := analyticsWithData()
	a.TotalReasoning = 0
	html := renderComponent(t, OverviewPage(p, a, DefaultRange, nil, nil))

	if strings.Contains(html, `<dt>Reasoning</dt>`) {
		t.Error("tokens tooltip should omit the reasoning line when there are no reasoning tokens")
	}
}

// The overview drops the recent-activity feed and the redundant scope subtitle:
// the panel is the page.
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

// The overview's By user breakdown only earns its place once more than one user has
// usage in the scope: a single-user instance (or a single-user filter, like the public
// overview) gains nothing from a breakdown of one row.
func TestOverviewPageByUserBreakdownGatedOnMultipleUsers(t *testing.T) {
	p := Page{Title: "Overview", LoggedIn: true, Active: "overview", Username: "Grace Hopper"}

	one := analyticsWithData()
	one.Users = []store.Breakdown{{Label: "grace", CostUSD: 12.5, Input: 100, Sessions: 3}}
	html := renderComponent(t, OverviewPage(p, one, DefaultRange, nil, nil))
	if strings.Contains(html, "By user") {
		t.Error("a single-user breakdown should not render the By user list")
	}

	two := analyticsWithData()
	two.Users = []store.Breakdown{
		{Label: "grace", CostUSD: 9.0, Input: 80, Sessions: 2},
		{Label: "ada", CostUSD: 3.5, Input: 20, Sessions: 1},
	}
	html = renderComponent(t, OverviewPage(p, two, DefaultRange, nil, nil))
	for _, want := range []string{"By user", ">grace<", ">ada<"} {
		if !strings.Contains(html, want) {
			t.Errorf("a multi-user breakdown should render %q", want)
		}
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
			TotalCostUSD: 1.25, Visibility: "public", LastActiveAt: &ts,
		},
		ProjectID: 4, ProjectKey: "scratch", ProjectName: "scratch", ProjectKind: "standalone",
	}}
	html := renderComponent(t, GlobalSessionList(rows, store.SessionFilter{Sort: "updated", Desc: true}, SessionFooter{Shown: 1}))

	for _, want := range []string{
		// The whole row links to the session.
		`data-row-href="/sessions/7"`,
		// The row carries the agent, project, branch, and both state chips.
		`>claude</span>`, `>scratch</span>`, `>main</span>`,
		`class="tag standalone"`, `class="tag public"`,
		// Tokens: compact figure plus the breakdown card and cost.
		`160 tokens`, "<dt>In</dt>", "<dt>Out</dt>", "<dt>Cache read</dt>", "<dt>Cache write</dt>", "$1.25",
		// Time reads as the clock with the exact stamp (with its zone) on hover.
		`title="` + FmtTimeLong(context.Background(), &ts) + `"`,
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

// A feed row renders its first-prompt title as a muted second line when no search
// snippet is present, and no second line at all when the session has no title.
func TestGlobalSessionListTitleLine(t *testing.T) {
	ts := time.Now().UTC()
	rows := []store.SessionRow{
		{SessionSummary: store.SessionSummary{ID: 1, Agent: "claude", MessageCount: 2, Title: "Fix the timezone pass", LastActiveAt: &ts}, ProjectID: 1, ProjectKey: "akari", ProjectName: "akari", ProjectKind: "remote"},
		{SessionSummary: store.SessionSummary{ID: 2, Agent: "claude", MessageCount: 1, LastActiveAt: &ts}, ProjectID: 1, ProjectKey: "akari", ProjectName: "akari", ProjectKind: "remote"},
	}
	html := renderComponent(t, GlobalSessionList(rows, store.SessionFilter{Sort: "updated", Desc: true}, SessionFooter{Shown: 2}))

	if !strings.Contains(html, `class="srow-sub" title="Fix the timezone pass">Fix the timezone pass</div>`) {
		t.Errorf("a titled row should render its title as the second line, got:\n%s", html)
	}
	// The untitled row (id 2) renders no snippet class and no title sub-line for it;
	// exactly one srow-sub should appear (the titled row's).
	if n := strings.Count(html, `class="srow-sub"`); n != 1 {
		t.Errorf("exactly one title sub-line expected, got %d", n)
	}
}

// A searched feed renders the snippet as the second line with the match wrapped in
// <mark>, and the <mark> is template structure around escaped text: a snippet whose
// content or match contains markup renders escaped, never injected. The snippet
// replaces the title line when both would apply.
func TestGlobalSessionListSnippetLine(t *testing.T) {
	ts := time.Now().UTC()
	// A snippet whose surrounding text and matched run both contain markup-looking
	// text, to prove the template escapes every part and only the <mark> is real.
	snip := store.SearchSnippet{Text: "before <b>x</b> <script>alert(1)</script> after", MatchStart: 16, MatchEnd: 41}
	rows := []store.SessionRow{{
		SessionSummary: store.SessionSummary{ID: 9, Agent: "claude", MessageCount: 3, Title: "should be replaced by snippet", LastActiveAt: &ts},
		ProjectID:      1, ProjectKey: "akari", ProjectName: "akari", ProjectKind: "remote",
		Search: snip,
	}}
	html := renderComponent(t, GlobalSessionList(rows, store.SessionFilter{Query: "script", Sort: "updated", Desc: true}, SessionFooter{Shown: 1}))

	// The <mark> wrapper is real template markup.
	if !strings.Contains(html, "<mark>") || !strings.Contains(html, "</mark>") {
		t.Errorf("snippet should wrap the match in <mark>, got:\n%s", html)
	}
	// The content's own angle brackets are escaped, never emitted as elements.
	if strings.Contains(html, "<script>alert(1)</script>") || strings.Contains(html, "<b>x</b>") {
		t.Errorf("snippet content must be escaped, not injected as markup, got:\n%s", html)
	}
	if !strings.Contains(html, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("snippet content should render as escaped text, got:\n%s", html)
	}
	// The snippet replaces the title line: the title text must not appear.
	if strings.Contains(html, "should be replaced by snippet") {
		t.Error("a searched row should show the snippet, not the title")
	}
}

// The footer renders "Showing N" with a "Show more" htmx control when more rows match
// than the page holds, "N sessions" (the exact total) when the page is the whole set,
// and the terse empty-hidden toggle. At the cap it names the cap instead of a button.
func TestGlobalSessionListFooter(t *testing.T) {
	ts := time.Now().UTC()
	rows := []store.SessionRow{{
		SessionSummary: store.SessionSummary{ID: 1, Agent: "claude", MessageCount: 1, LastActiveAt: &ts},
		ProjectID:      1, ProjectKey: "akari", ProjectName: "akari", ProjectKind: "remote",
	}}
	// hasMore true: the page is not the whole set, so the count reads "Showing N", a
	// "Show more" control appears, and the empty toggle (hasEmpty true) reads "empty
	// hidden · show".
	sel := store.SessionFilter{Sort: "updated", Desc: true, Limit: 100}
	footer := BuildSessionFooter(sel, 100, true, true)
	html := renderComponent(t, GlobalSessionList(rows, sel, footer))

	for _, want := range []string{
		`Showing 100`,
		`hx-target="#session-list"`, `hx-select="#session-list"`, `hx-swap="outerHTML"`,
		`>Show more</a>`,
		// Empty toggle: hidden, a "show" verb, no count.
		`empty hidden`, `>show</a>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("footer missing %q, got:\n%s", want, html)
		}
	}
	if strings.Contains(html, " of ") {
		t.Errorf("the more-matching footer should not read 'N of M', got:\n%s", html)
	}

	// hasMore false: the shown count IS the exact total, so the footer reads "N
	// sessions" and offers no "Show more".
	exact := BuildSessionFooter(store.SessionFilter{Sort: "updated", Desc: true}, 7, false, false)
	exactHTML := renderComponent(t, GlobalSessionList(rows, store.SessionFilter{Sort: "updated", Desc: true}, exact))
	if !strings.Contains(exactHTML, "7 sessions") {
		t.Errorf("an exhausted page should read the exact 'N sessions', got:\n%s", exactHTML)
	}
	if strings.Contains(exactHTML, ">Show more</a>") {
		t.Errorf("an exhausted page should carry no Show more, got:\n%s", exactHTML)
	}

	// At the cap with more matching, the button is replaced by the cap note.
	capped := BuildSessionFooter(store.SessionFilter{Limit: 500}, 500, true, false)
	capHTML := renderComponent(t, GlobalSessionList(rows, store.SessionFilter{Limit: 500}, capped))
	if strings.Contains(capHTML, ">Show more</a>") {
		t.Error("at the cap there should be no Show more button")
	}
	if !strings.Contains(capHTML, "at cap") {
		t.Error("at the cap the footer should name the cap")
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
		{SessionSummary: store.SessionSummary{ID: 1, Agent: "claude", LastActiveAt: &today}, ProjectID: 1, ProjectKey: "akari", ProjectName: "akari", ProjectKind: "remote"},
		{SessionSummary: store.SessionSummary{ID: 2, Agent: "claude", LastActiveAt: &earlier}, ProjectID: 1, ProjectKey: "akari", ProjectName: "akari", ProjectKind: "remote"},
	}

	grouped := renderComponent(t, GlobalSessionList(rows, store.SessionFilter{Sort: "updated", Desc: true}, SessionFooter{Shown: 2}))
	if !strings.Contains(grouped, `class="day-head"`) || !strings.Contains(grouped, `>Today</span>`) {
		t.Error("most-recent order should render a day heading")
	}
	// The second row repeats the first row's project, so its label fades.
	if !strings.Contains(grouped, `class="srow-proj faded"`) {
		t.Error("a repeated project label should fade")
	}

	flat := renderComponent(t, GlobalSessionList(rows, store.SessionFilter{Sort: "tokens", Desc: true}, SessionFooter{Shown: 2}))
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
	html := renderComponent(t, ProjectPage(p, proj, nil, store.SessionRemainder{}, Facets{}, sel, analyticsWithData(), store.Insights{}, "90d"))

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
	// An empty Insights (no graded sessions in the window) renders no Quality band, so
	// a project with nothing to grade shows the usage panel and the table with no empty
	// row of zero bars between them.
	if strings.Contains(html, `class="proj-quality"`) {
		t.Error("project page with empty Insights should render no Quality band")
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

// With a populated Insights the project page grows a Quality band: the section label, the
// three distribution panels, and the tools and churn panels, with the grade and outcome
// drill-downs scoped to the project (and any active filter). The band is a lean subset of
// /insights, so it deliberately omits the velocity, concurrency, hygiene, and context bands,
// and its churn rows drop the per-bar project tag since every row is this one project.
func TestProjectPageRendersQualityBand(t *testing.T) {
	p := Page{Title: "akari", LoggedIn: true, Active: "projects", Username: "Anna Winlock"}
	proj := store.ProjectSummary{ID: 7, RemoteKey: "hopper/akari", Kind: "remote", SessionCount: 15}
	// The base scope carries the project and an active agent filter, so the drill-down
	// hrefs must fold both in beside the bucket.
	sel := store.SessionFilter{ProjectID: 7, Agent: "claude"}
	html := renderComponent(t, ProjectPage(p, proj, nil, store.SessionRemainder{}, Facets{}, sel, analyticsWithData(), sampleInsights(), "90d"))

	for _, want := range []string{
		// The band and its section label, plus the caption naming its window semantics (the
		// band windows on started_at, matching the Insights convention, distinct from the
		// usage panel's usage-event window; documented here and at the handler call).
		`class="proj-quality"`, `>Quality</span>`,
		`sessions that started in this window`,
		// The three distribution panels reuse the Insights grid.
		`class="ins-grid"`, `>Grades<`, `>Outcomes<`, `>Archetypes<`,
		// The coverage note rides the Grades head (11 of 15 graded reads 73%).
		`73% graded`,
		// A grade drill-down carries the project scope, the active agent filter, the bucket, the
		// active range, AND empty=1, so it lands on the same sessions the bar counts, bounded to the
		// same window and under the same empty-session policy rather than opening the all-time feed.
		`href="/sessions?agent=claude&amp;empty=1&amp;grade=A&amp;project=7&amp;range=90d"`,
		// An outcome drill-down likewise carries the project scope, the filter, the range, and empty=1.
		`href="/sessions?agent=claude&amp;empty=1&amp;outcome=completed&amp;project=7&amp;range=90d"`,
		// The tools and churn panels round out the band.
		`>Tools<`, `>File churn<`, `>Read<`,
		// The churn caption drops the fleet page's "grouped per project" clause.
		`files edited more than once in this window<`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("project quality band missing %q", want)
		}
	}
	// The band stays lean: none of the fleet-only bands appear on the project page.
	for _, unwanted := range []string{`>Velocity<`, `>Concurrency<`, `>Prompt hygiene<`, `>Context health<`} {
		if strings.Contains(html, unwanted) {
			t.Errorf("project quality band should omit the fleet-only band %q", unwanted)
		}
	}
	// Every churn row is this one project, so the per-bar project tag is dropped as noise.
	if strings.Contains(html, `class="churn-proj"`) {
		t.Error("project quality churn should drop the per-bar project tag")
	}
}
