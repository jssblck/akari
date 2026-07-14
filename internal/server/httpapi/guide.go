package httpapi

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/jssblck/akari/internal/guide"
	"github.com/jssblck/akari/internal/server/web"
)

// The user guide is public: a logged-out visitor and a coding agent must both be
// able to read it, so these handlers carry no auth gate. It is static content
// independent of the parsed projection, so it is not behind the reparse gate
// either. handleGuideIndex serves /guide; handleGuidePage serves both the HTML
// chapter at /guide/<slug> and its raw Markdown at /guide/<slug>.md; the two
// llms endpoints serve the machine-readable index and the whole guide as one
// file.

// handleGuideIndex serves the guide overview at /guide.
func (s *Server) handleGuideIndex(w http.ResponseWriter, r *http.Request) {
	s.serveGuideChapter(w, r, "index")
}

// handleGuidePage serves a chapter at /guide/<slug>, or its raw Markdown when the
// slug carries a .md suffix. The suffix is split here rather than routed
// separately, since both forms live under the one {slug} path segment.
func (s *Server) handleGuidePage(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	if raw := strings.TrimSuffix(slug, ".md"); raw != slug {
		s.serveGuideRaw(w, r, raw)
		return
	}
	s.serveGuideChapter(w, r, slug)
}

// serveGuideChapter renders one chapter in the docs layout. An unknown slug is a
// public 404, a render failure a public 500, so a broken chapter surfaces as an
// error rather than a blank page.
func (s *Server) serveGuideChapter(w http.ResponseWriter, r *http.Request, slug string) {
	c, ok := guide.Lookup(slug)
	if !ok {
		renderPublicError(w, r, http.StatusNotFound, "That guide page does not exist.")
		return
	}
	rendered, err := c.Render()
	if err != nil {
		renderPublicError(w, r, http.StatusInternalServerError, "Could not render the guide.")
		return
	}
	raw, err := c.Raw()
	if err != nil {
		renderPublicError(w, r, http.StatusInternalServerError, "Could not load the guide.")
		return
	}

	view := web.GuideView{
		Title:       c.Title,
		Summary:     c.Summary,
		BodyHTML:    string(rendered.HTML),
		RawMarkdown: raw,
		RawPath:     c.RawRoute(),
		GithubURL:   c.GitHubURL(),
		Nav:         guideNav(c.Slug),
		Toc:         guideTocItems(rendered.Headings),
		LoggedIn:    s.guideViewerLoggedIn(w, r),
	}
	prev, next := c.Neighbors()
	if prev != nil {
		view.Prev = &web.GuideLink{Label: guideNavLabel(*prev), Route: prev.Route()}
	}
	if next != nil {
		view.Next = &web.GuideLink{Label: guideNavLabel(*next), Route: next.Route()}
	}
	render(w, r, http.StatusOK, web.GuidePage(view))
}

// serveGuideRaw serves a chapter's raw Markdown: the representation agents and
// crawlers probe for and the exact text "Copy page" copies.
func (s *Server) serveGuideRaw(w http.ResponseWriter, r *http.Request, slug string) {
	c, ok := guide.Lookup(slug)
	if !ok {
		http.NotFound(w, r)
		return
	}
	raw, err := c.Raw()
	if err != nil {
		http.Error(w, "Could not load the guide.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(raw))
}

// handleLLMsTxt serves the llms.txt discovery index (https://llmstxt.org): the
// chapters, each linked to its raw Markdown, so an agent learns the guide's shape
// in one fetch.
func (s *Server) handleLLMsTxt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(guide.LLMsTxt(s.absURL(r, ""))))
}

// handleLLMsFullTxt serves llms-full.txt: every chapter concatenated in reading
// order, so an agent ingests the whole guide in a single request.
func (s *Server) handleLLMsFullTxt(w http.ResponseWriter, r *http.Request) {
	body, err := guide.LLMsFullTxt(s.absURL(r, ""))
	if err != nil {
		http.Error(w, "Could not load the guide.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(body))
}

// guideViewerLoggedIn reports whether the request carries a full-scope credential
// (a browser session in practice), which switches the docs header's corner action
// from "Log in" to "Open app". The guide itself is readable regardless.
func (s *Server) guideViewerLoggedIn(w http.ResponseWriter, r *http.Request) bool {
	p, ok := s.resolve(r)
	loggedIn := ok && p.Scope == scopeFull
	if loggedIn {
		setPrivateNoStore(w)
	}
	return loggedIn
}

// guideNav builds the sidebar rail from the chapter registry, marking the active
// chapter and zero-padding each reading position.
func guideNav(active string) []web.GuideNavItem {
	cs := guide.Chapters()
	items := make([]web.GuideNavItem, 0, len(cs))
	for _, c := range cs {
		items = append(items, web.GuideNavItem{
			Num:    fmt.Sprintf("%02d", c.Order),
			Label:  guideNavLabel(c),
			Route:  c.Route(),
			Active: c.Slug == active,
		})
	}
	return items
}

// guideNavLabel is a chapter's rail label: the index reads "Overview" rather than
// its long document title, every other chapter its own title.
func guideNavLabel(c guide.Chapter) string {
	if c.IsIndex() {
		return "Overview"
	}
	return c.Title
}

// guideTocItems maps the rendered headings to the on-this-page rail's view model.
func guideTocItems(hs []guide.Heading) []web.GuideTocItem {
	items := make([]web.GuideTocItem, 0, len(hs))
	for _, h := range hs {
		items = append(items, web.GuideTocItem{ID: h.ID, Text: h.Text, Level: h.Level})
	}
	return items
}
