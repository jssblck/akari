package web

import (
	"strings"
	"testing"
)

// The logged-out landing page rides the public layout (its top bar carries the
// wordmark and a Log in link) and pitches akari in a hero with paths into
// sign-in and registration. Pinning the wrapper and those links at the source
// package guards the anonymous root's entry points independently of the httpapi
// route wiring.
func TestLandingPageRendersHeroAndEntryPoints(t *testing.T) {
	html := renderComponent(t, LandingPage())

	// The public layout wraps it: the top bar's brand and Log in link, and the
	// page title carries the product name.
	for _, want := range []string{
		`<title>akari - akari</title>`,
		`class="topbar"`,
		`<a href="/login">Log in</a>`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("landing page should ride the public layout; missing %q", want)
		}
	}

	// The hero explains what akari is and offers both a sign-in and a registration
	// action, so a first-time visitor can act without hunting for a link.
	for _, want := range []string{
		`self-hosted instrument`,
		`class="btn" href="/login"`,
		`class="btn secondary" href="/register"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("landing hero missing %q", want)
		}
	}

	// It is the logged-out shell, so it must not render the signed-in app chrome
	// (the sidebar and its Account nav).
	if strings.Contains(html, `class="sidebar"`) {
		t.Error("landing page should not render the signed-in sidebar")
	}
}
