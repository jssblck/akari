package web

import (
	"strings"
	"testing"
)

// sampleGuideView is a representative docs view: a middle chapter with a prev and
// a next, a couple of TOC headings, and a corner action, so one render exercises
// every part of the layout.
func sampleGuideView() GuideView {
	return GuideView{
		Title:       "The client",
		Summary:     "The akari CLI in depth.",
		BodyHTML:    `<h1>The client</h1><h2 id="commands">Commands</h2><p>Driving the CLI.</p><h2 id="discovery">Discovery</h2><p>Finding sessions.</p>`,
		RawMarkdown: "# The client\n\nThe akari CLI in depth.\n",
		RawPath:     "/guide/the-client.md",
		GithubURL:   "https://github.com/jssblck/akari/blob/main/internal/guide/content/the-client.md",
		Nav: []GuideNavItem{
			{Num: "00", Label: "Overview", Route: "/guide", Active: false},
			{Num: "03", Label: "The client", Route: "/guide/the-client", Active: true},
		},
		Toc: []GuideTocItem{
			{ID: "commands", Text: "Commands", Level: 2},
			{ID: "discovery", Text: "Discovery", Level: 2},
		},
		Prev: &GuideLink{Label: "Getting started", Route: "/guide/getting-started"},
		Next: &GuideLink{Label: "The web UI", Route: "/guide/the-web-ui"},
	}
}

// The docs page must carry its own head (rel=alternate for the raw Markdown and
// llms.txt, the guide stylesheet and script) and the full three-column frame, so
// a browsing reader and an agent both get what they came for.
func TestGuidePageRendersDocsLayout(t *testing.T) {
	html := renderComponent(t, GuidePage(sampleGuideView()))

	for _, want := range []string{
		`<title>The client - akari</title>`,
		`<link rel="alternate" type="text/markdown" href="/guide/the-client.md">`,
		`<link rel="alternate" type="text/plain" title="llms.txt" href="/llms.txt">`,
		`href="/static/guide.css"`,
		`src="/static/guide.js"`,
		// The rendered chapter body is injected verbatim (its heading anchor intact).
		`<h2 id="commands">Commands</h2>`,
		// Sidebar rail with the active chapter marked.
		`class="guide-nav-link is-active"`,
		`aria-current="page"`,
		// Page actions for humans and agents.
		`data-copy-page`,
		`href="/guide/the-client.md"`,
		`href="https://github.com/jssblck/akari/blob/main/internal/guide/content/the-client.md"`,
		// TOC rail with scroll-spy hooks.
		`data-guide-toc="commands"`,
		// Prev/next footer.
		`Getting started`,
		`The web UI`,
		// The raw Markdown is embedded once for the copy action.
		`id="guide-page-markdown"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("guide page missing %q", want)
		}
	}
}

// The TOC is suppressed when a chapter has fewer than two headings, where a
// table of contents adds nothing.
func TestGuideTocSuppressedWhenTrivial(t *testing.T) {
	v := sampleGuideView()
	v.Toc = []GuideTocItem{{ID: "only", Text: "Only", Level: 2}}
	html := renderComponent(t, GuidePage(v))
	if strings.Contains(html, `class="guide-toc"`) {
		t.Errorf("single-heading chapter should not render the TOC rail")
	}
}

// A logged-out reader gets a "Log in" corner action; a signed-in one gets "Open
// app", since the guide is reachable in either state.
func TestGuideHeaderAction(t *testing.T) {
	out := renderComponent(t, GuidePage(sampleGuideView()))
	if !strings.Contains(out, `href="/login"`) {
		t.Errorf("logged-out guide header should offer Log in")
	}
	v := sampleGuideView()
	v.LoggedIn = true
	in := renderComponent(t, GuidePage(v))
	if !strings.Contains(in, `>Open app</a>`) {
		t.Errorf("signed-in guide header should offer Open app")
	}
}
