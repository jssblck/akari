package web

// The logged-out landing page mocks up the app's own surfaces with numbers that
// never touched the store: no session backs the "412 sessions" row, no
// usage_events sum into the "71%" cache-hit tile. Because those figures are
// invented, nothing enforces the projection-consistency property the live
// pages get for free from a single query (a strip total that is exactly the
// sum of the rows underneath it, a token class breakdown that is exactly the
// tokens in the headline figure). A literal typed directly into landing.templ
// in four places (the strip, the row, the remainder footer, the hover card)
// can drift the moment one of the four is edited and the others are not.
//
// This file gives the mock one canonical data source instead: the project
// rows and the remainder are the only hand-picked numbers, and every other
// figure the page renders (the strip's totals, the cache-hit percent, the
// per-row token cell and its hover breakdown) is derived from them in Go, so
// an edit to a mock number has one place to happen and cannot leave the page
// showing two versions of the same figure.
type landingMockProject struct {
	Name                           string
	Sessions                       int64
	In, Out, CacheRead, CacheWrite int64
	Cost                           float64
	Spark                          string // SVG polyline points; empty for none.
}

// Tokens sums the four token classes, so the total rendered in a tok-cell
// headline can never disagree with the breakdown in its hover card: both
// read from the same four fields.
func (p landingMockProject) Tokens() int64 {
	return p.In + p.Out + p.CacheRead + p.CacheWrite
}

// landingMockProjects are the three visible rows of the cost mock's table.
// Values are hand-picked to look like a real project ledger; every other
// figure the page shows for these projects (or their aggregate) derives from
// this slice rather than repeating a literal.
var landingMockProjects = []landingMockProject{
	{
		Name:       "jssblck/akari",
		Sessions:   412,
		In:         7_400_000,
		Out:        3_100_000,
		CacheRead:  19_300_000,
		CacheWrite: 1_400_000,
		Cost:       148.11,
		Spark:      "0,12 12,10 24,11 36,6 48,7 64,3",
	},
	{
		Name:       "jssblck/tapestry",
		Sessions:   188,
		In:         3_200_000,
		Out:        1_300_000,
		CacheRead:  7_900_000,
		CacheWrite: 500_000,
		Cost:       61.40,
		Spark:      "0,6 12,8 24,7 36,9 48,8 64,10",
	},
	{
		Name:       "hopper/subroutines",
		Sessions:   97,
		In:         2_100_000,
		Out:        800_000,
		CacheRead:  5_000_000,
		CacheWrite: 400_000,
		Cost:       37.92,
		Spark:      "0,9 12,7 24,8 36,7 48,5 64,6",
	},
}

// landingMockRemainder is the capped-table footer row: the app's own idiom for
// folding every project past the visible cutoff into one summary row, so the
// mock's strip totals can reconcile against a table that only ever shows a
// handful of rows. It carries no spark line because it is not one project's
// trend.
var landingMockRemainder = landingMockProject{
	Name:       "all other projects",
	Sessions:   587,
	In:         11_300_000,
	Out:        4_000_000,
	CacheRead:  26_600_000,
	CacheWrite: 2_100_000,
	Cost:       165.44,
}

// landingMockTotals sums the visible rows and the remainder into the one
// aggregate the strip renders from, so the strip's Cost, Tokens, and Sessions
// tiles cannot disagree with the table beneath them: they are the same sum.
//
// Costs are summed in integer cents rather than float64 dollars: the four
// mock costs (148.11, 61.40, 37.92, 165.44) happen to sum cleanly in float64
// too, but cents avoid relying on that and keep FmtCost's "%.2f" exact for any
// future edit to the mock figures.
func landingMockTotals() landingMockProject {
	var totals landingMockProject
	cents := int64(0)
	for _, p := range append(append([]landingMockProject{}, landingMockProjects...), landingMockRemainder) {
		totals.Sessions += p.Sessions
		totals.In += p.In
		totals.Out += p.Out
		totals.CacheRead += p.CacheRead
		totals.CacheWrite += p.CacheWrite
		cents += int64(p.Cost*100 + 0.5)
	}
	totals.Cost = float64(cents) / 100
	return totals
}

// landingMockCacheHit is the fraction of input tokens served from cache
// across the totals (cache reads over cache reads plus fresh input), the same
// ratio the live Cache tile computes from usage_events. Deriving it from
// landingMockTotals rather than hand-picking "71%" means the strip's
// Tokens tile and Cache hit tile can never describe two different corpora.
func landingMockCacheHit() float64 {
	totals := landingMockTotals()
	denom := totals.In + totals.CacheRead
	if denom == 0 {
		return 0
	}
	return float64(totals.CacheRead) / float64(denom)
}

// landingMockFacet is one row of a facet rail group: a label (an agent name, a
// machine hostname, a user) and the session count filtering to it would show.
type landingMockFacet struct {
	Name  string
	Count int64
}

// landingMockFacetGroup is one column of the facet rail (Agent, Machine, or
// User), each a different partition of the same session population.
type landingMockFacetGroup struct {
	Label string
	Rows  []landingMockFacet
}

// landingMockFacets partitions the same 1,284 sessions three different ways,
// the way the real facet rail always sums a filter's rows back to the
// unfiltered session count regardless of which dimension you slice by. The
// reconciliation test pins each group's sum against landingMockTotals so an
// edit to one group (or to the project rows the total derives from) that
// breaks the story fails loudly.
var landingMockFacets = []landingMockFacetGroup{
	{
		Label: "Agent",
		Rows: []landingMockFacet{
			{Name: "claude", Count: 812},
			{Name: "codex", Count: 341},
			{Name: "pi", Count: 131},
		},
	},
	{
		Label: "Machine",
		Rows: []landingMockFacet{
			{Name: "framework", Count: 623},
			{Name: "mac-studio", Count: 512},
			{Name: "thinkpad", Count: 149},
		},
	},
	{
		Label: "User",
		Rows: []landingMockFacet{
			{Name: "grace", Count: 704},
			{Name: "ada", Count: 391},
			{Name: "katherine", Count: 189},
		},
	},
}
