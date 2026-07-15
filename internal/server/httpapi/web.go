package httpapi

import (
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/jssblck/akari/internal/server/ogimage"
	"github.com/jssblck/akari/internal/server/web"
)

// requireBrowserRead preserves browser navigation semantics for non-JSON reads:
// unauthenticated clients go through the React login route instead of receiving
// a JSON error.
func (s *Server) requireBrowserRead(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setPrivateNoStore(w)
		p, ok := s.resolve(r)
		if !ok || p.Scope != scopeFull {
			http.Redirect(w, r, s.loginRedirect(r), http.StatusSeeOther)
			return
		}
		next(w, s.withPrincipal(r, p))
	}
}

// loginRedirect builds the login bounce for an unauthenticated browser
// navigation. Both the login page path and the next destination it carries are
// externalized (prefixed) here, because the browser and the SPA resolve them
// against the external URL space, not the stripped internal one.
func (s *Server) loginRedirect(r *http.Request) string {
	return s.href(r, "/login?next="+url.QueryEscape(s.href(r, r.URL.RequestURI())))
}

// render writes a templ component with the given status. The status is committed
// before the body streams, so a mid-render failure cannot be remapped to a 500;
// it only truncates the response, which the browser surfaces as a broken page.
// Buffering the render to recover a clean 500 is deliberately not done: these
// pages are cheap to render and effectively never fail.
func render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	// The external path prefix rides the render context, so every templated href,
	// form action, and asset link externalizes through web.Href and web.StaticURL
	// without each handler threading it.
	r = r.WithContext(web.WithBasePath(r.Context(), requestPrefix(r)))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = c.Render(r.Context(), w)
}

// renderPublicError writes the logged-out error page used by public share links and
// the guide, where no viewer chrome exists to wrap the message.
func renderPublicError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	render(w, r, status, web.PublicErrorPage(status, msg))
}

// handleRoot serves the site root at /: the marketing landing page explaining
// what akari is, shown to every visitor regardless of sign-in state. A signed-in
// reader gets the same page with a topbar that points back into the app (an
// Overview link in place of "Log in"), so the homepage stays reachable while
// logged in; the app itself lives at /overview. The page is never gated during a
// reparse, since it renders no parsed data.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	// The meta copy derives from the ogimage package's canonical landing
	// constants (the same strings the /og.png card draws), so a copy edit
	// cannot leave the page's tags and its preview image saying different
	// things. The title lowercases the headline into the product register the
	// overview card's title uses ("<subject> · <what it is>").
	og := web.OGMeta{
		Title:       "akari · " + strings.ToLower(strings.TrimSuffix(ogimage.LandingHeadline, ".")),
		Description: ogimage.LandingSubline,
		URL:         s.absURL(r, "/"),
		Image:       s.absURL(r, "/og.png"),
	}
	// A full-scope reader gets a route back into the application. The landing page
	// does not need the account record itself.
	loggedIn := false
	if p, ok := s.resolve(r); ok && p.Scope == scopeFull {
		setPrivateNoStore(w)
		loggedIn = true
	}
	render(w, r, http.StatusOK, web.LandingPage(og, loggedIn))
}

// dashboardCacheMaxAge lets a browser reuse a dashboard response for a few seconds,
// so back/forward navigation and range-selector refetches do not re-run the query
// pipeline. It is private (these responses carry the viewer's own windowed figures)
// and short enough that a reader who pauses sees fresh numbers. The server-side
// insights cache absorbs the cross-viewer and post-expiry load behind it. The
// sessions feed is deliberately excluded: it is already a single indexed query and
// changes on every ingest, so a stale feed would mislead.
const dashboardCacheMaxAge = 30

func setDashboardCache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(dashboardCacheMaxAge))
}

// handleNotFound gives unknown paths the public error shell. It performs no
// account lookup because an error page exposes no private state.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	render(w, r, http.StatusNotFound, web.PublicErrorPage(http.StatusNotFound, "That page does not exist."))
}

// handleSessionEvents is the SSE endpoint that signals a watching browser to
// re-fetch the session body when the session gains newly parsed bytes.
func (s *Server) handleSessionEvents(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Confirm the session exists before holding a long-lived connection open.
	if _, err := s.Store.SessionDetailByID(r.Context(), id); err != nil {
		http.NotFound(w, r)
		return
	}

	ch := s.hub.subscribe(id)
	defer s.hub.unsubscribe(id, ch)
	serveSSE(w, r, ch, func(struct{}) string { return "event: update\ndata: 1\n\n" }, func(write func(string) bool) bool {
		return write("event: update\ndata: 1\n\n")
	})
}

// staticHandler serves the embedded static assets under /static/.
func staticHandler() http.Handler {
	sub, err := fs.Sub(web.Static, "static")
	if err != nil {
		panic(err)
	}
	return http.StripPrefix("/static/", http.FileServer(http.FS(sub)))
}

// faviconICO is the embedded favicon read once at startup: browsers request
// /favicon.ico at the site root unprompted (before, and regardless of, the
// <link> tags), so serving it there keeps that automatic hit from 404ing. It is
// the same bytes as /static/favicon.ico; a missing file is a build error, so the
// read panics rather than degrading silently.
var faviconICO = func() []byte {
	b, err := web.Static.ReadFile("static/favicon.ico")
	if err != nil {
		panic(err)
	}
	return b
}()

// handleFaviconICO serves the legacy .ico at the root path browsers probe for a
// tab icon. The bytes are static per binary, so the response is aggressively
// cacheable like the landing card.
func (s *Server) handleFaviconICO(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/x-icon")
	w.Header().Set("Content-Length", strconv.Itoa(len(faviconICO)))
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", landingOGCacheMaxAge))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(faviconICO)
}
