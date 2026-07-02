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
	html := renderComponent(t, LandingPage())

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
