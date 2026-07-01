package web

import (
	"strings"
	"testing"
)

// The logged-out landing page rides the public layout, whose top bar carries the
// wordmark plus the Docs, GitHub, and Log in links. The hero is explanation, not
// a call to action. Pinning the wrapper and the top-bar entry points at the
// source package guards the anonymous root independently of the httpapi route
// wiring.
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

	// The hero explains what akari is.
	if !strings.Contains(html, `self-hosted instrument`) {
		t.Errorf("landing hero should explain what akari is")
	}

	// The prominent hero buttons and the first-account note were removed, so the
	// hero carries no call-to-action buttons and the foot makes no admin claim.
	for _, unwanted := range []string{
		`class="btn" href="/login"`,
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
