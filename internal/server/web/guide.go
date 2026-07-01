package web

// The guide (user documentation) renders in its own docs layout rather than the
// signed-in app shell: it is public, so a logged-out visitor and a coding agent
// can both reach it. The httpapi layer builds a GuideView from the guide package
// (chapter registry plus rendered Markdown) and hands it to GuidePage; these
// types keep the web layer free of any dependency on the guide package's own
// types, so this package stays pure presentation.

// GuideNavItem is one chapter in the docs sidebar rail.
type GuideNavItem struct {
	// Num is the zero-padded reading position ("00".."08"), shown in mono beside
	// the label.
	Num    string
	Label  string
	Route  string
	Active bool
}

// GuideTocItem is one heading in the on-this-page table of contents. Level is 2
// or 3; an H3 is indented under the H2 it follows.
type GuideTocItem struct {
	ID    string
	Text  string
	Level int
}

// GuideLink is a prev/next navigation target.
type GuideLink struct {
	Label string
	Route string
}

// GuideView is everything the docs page needs to render one chapter.
type GuideView struct {
	Title   string
	Summary string
	// BodyHTML is the chapter rendered to HTML by the guide package's goldmark
	// pipeline over our own trusted Markdown, injected verbatim into the prose
	// column.
	BodyHTML string
	// RawMarkdown is the exact Markdown source, embedded once so "Copy page" can
	// place it on the clipboard without a network round-trip.
	RawMarkdown string
	// RawPath is the chapter's raw-Markdown URL (/guide/<slug>.md), the "View as
	// Markdown" target and the rel=alternate the head advertises.
	RawPath   string
	GithubURL string
	Nav       []GuideNavItem
	Toc       []GuideTocItem
	Prev      *GuideLink
	Next      *GuideLink
	// LoggedIn switches the header's corner action between "Open app" and
	// "Log in", since the guide is reachable in either state.
	LoggedIn bool
}
