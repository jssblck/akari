package web

import (
	"strings"
	"testing"
)

// The logged-out landing page is a product landing page: a hero over alternating
// copy-and-mock sections, a capabilities definition list, and a quickstart terminal
// block, all riding the public layout. Its top bar carries the wordmark plus the
// Docs, GitHub, and Log in links; the hero's CTAs are link-buttons to the guide and
// the repository. Pinning the wrapper, the top-bar entry points, and the page's
// spine at the source package guards the anonymous root independently of the httpapi
// route wiring.
func TestLandingPageRendersHeroAndEntryPoints(t *testing.T) {
	html := renderComponent(t, LandingPage(OGMeta{}))

	// The public layout wraps it: the top bar's brand, the Docs and GitHub links,
	// the Log in link, and the product name in the page title.
	for _, want := range []string{
		`<title>akari - akari</title>`,
		`class="topbar"`,
		`href="/guide">Docs`,
		`aria-label="akari on GitHub"`,
		`<a href="/login">Log in</a>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("landing page should ride the public layout; missing %q", want)
		}
	}

	// The hero explains what akari is and keeps the pinned phrase.
	if !strings.Contains(html, `Know what your agents actually did.`) {
		t.Errorf("landing hero should carry its headline")
	}
	if !strings.Contains(html, `self-hosted instrument`) {
		t.Errorf("landing hero should explain what akari is")
	}

	// The hero CTAs are link-buttons: the primary goes to the guide, the secondary to
	// the repository. The guide CTA is the primary action, not a login button.
	if !strings.Contains(html, `class="btn" href="/guide"`) {
		t.Errorf("hero should carry the primary guide CTA")
	}

	// The spine: one section heading, the quickstart command, and a capabilities term
	// prove the mock-driven sections rendered.
	for _, want := range []string{
		`Every machine, one history`,
		`akari sync`,
		`Read it from your agent`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("landing page should render its spine; missing %q", want)
		}
	}

	// The cost mock's token figures ride the shared tok-cell + tokenCard treatment
	// (never a bare number), and the table closes with the remainder footer that
	// makes the strip's totals reconcile with the visible rows.
	for _, want := range []string{
		`class="tok-cell"`,
		`class="tok-tip"`,
		`class="remainder"`,
		`all other projects`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("landing cost mock should carry the shared token treatment; missing %q", want)
		}
	}

	// The old prominent register/login buttons and the first-account note were never
	// carried over: the hero CTA goes to the guide, and the foot makes no admin claim.
	for _, unwanted := range []string{
		`class="btn secondary" href="/register"`,
		`needs no invite and becomes the admin`,
	} {
		if strings.Contains(html, unwanted) {
			t.Errorf("landing should no longer render %q", unwanted)
		}
	}

	// It is the logged-out shell, so it must not render the signed-in app chrome
	// (the sidebar and its Account nav).
	if strings.Contains(html, `class="sidebar"`) {
		t.Error("landing page should not render the signed-in sidebar")
	}
}

// TestLandingMockDataReconciles pins the projection-consistency property the
// landing mock is required to have even though nothing behind it is a real
// row: the facet rail's three groups, the project table, and the strip all
// describe the same population of sessions. It also pins the rendered
// figures a viewer actually sees, so an edit to a project row or a facet
// count that breaks the story (a group that no longer sums to the session
// total, a total that no longer matches the headline strip) fails loudly
// instead of silently drifting.
func TestLandingMockDataReconciles(t *testing.T) {
	totals := landingMockTotals()

	for _, group := range landingMockFacets {
		var sum int64
		for _, row := range group.Rows {
			sum += row.Count
		}
		if sum != totals.Sessions {
			t.Errorf("facet group %q sums to %d sessions, want %d (landingMockTotals().Sessions)", group.Label, sum, totals.Sessions)
		}
	}

	if totals.Sessions != 1284 {
		t.Errorf("landingMockTotals().Sessions = %d, want 1284", totals.Sessions)
	}
	if got := FmtTokensCompact(totals.Tokens()); got != "96.4M" {
		t.Errorf("FmtTokensCompact(landingMockTotals().Tokens()) = %q, want %q", got, "96.4M")
	}
	if got := FmtCost(totals.Cost, false); got != "$412.87" {
		t.Errorf("FmtCost(landingMockTotals().Cost, false) = %q, want %q", got, "$412.87")
	}
	if got := FmtPercent(landingMockCacheHit()); got != "71%" {
		t.Errorf("FmtPercent(landingMockCacheHit()) = %q, want %q", got, "71%")
	}
}
