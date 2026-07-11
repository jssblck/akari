// Package web holds akari's server-rendered UI: templ templates and the small
// view-model helpers they use. Handlers in the httpapi package resolve auth,
// query the store, and render these templates, so all rendering lives here.
package web

import (
	"context"
	"embed"
	"fmt"
	"net/http"
	"time"
)

// errorTitle is the browser-tab title for a public error page: the status code
// paired with its standard reason ("404 Not Found"), so the tab and any shared
// link say what went wrong rather than a bare number. An unknown code with no
// standard text falls back to the number alone.
func errorTitle(code int) string {
	if text := http.StatusText(code); text != "" {
		return fmt.Sprintf("%d %s", code, text)
	}
	return fmt.Sprintf("%d", code)
}

// locCtxKey keys the viewer's timezone in the render context. The httpapi layer
// resolves it from the tz cookie and stashes it before rendering; the formatting
// helpers below read it back through Loc. An unexported type keeps the key from
// colliding with any other package's context values.
type locCtxKey struct{}

// WithLoc returns a context carrying the viewer's timezone, for the httpapi render
// path to attach before a component renders. A nil location is ignored so the
// accessor's UTC default still applies.
func WithLoc(ctx context.Context, loc *time.Location) context.Context {
	if loc == nil {
		return ctx
	}
	return context.WithValue(ctx, locCtxKey{}, loc)
}

// Loc is the viewer's timezone for the current render, or UTC when none was set.
// The formatting helpers localize every stamp and day heading through it, so a
// reader sees times in their own zone; a page rendered outside a request (or
// before the cookie is set) reads UTC. templ exposes the render ctx implicitly, so
// a template calls FmtTime(ctx, t) and this reads the zone off that ctx.
func Loc(ctx context.Context) *time.Location {
	if loc, ok := ctx.Value(locCtxKey{}).(*time.Location); ok && loc != nil {
		return loc
	}
	return time.UTC
}

// noticeCtxKey keys the one-shot notice banner text in the render context, the
// same seam Loc uses: the httpapi layer drains the notice cookie once, at the
// render seam every page shares, and stashes the text here rather than
// threading it through every pageFor/pageForNav call site. An unexported type
// keeps the key from colliding with any other package's context values.
type noticeCtxKey struct{}

// WithNotice returns a context carrying a one-shot notice banner's text, for the
// httpapi render path to attach before a component renders. An empty string is
// ignored, so the accessor's no-notice default still applies.
func WithNotice(ctx context.Context, notice string) context.Context {
	if notice == "" {
		return ctx
	}
	return context.WithValue(ctx, noticeCtxKey{}, notice)
}

// Notice is the current render's one-shot notice banner text, or "" when none was
// set. The authed layout renders it once at the top of main when non-empty.
func Notice(ctx context.Context) string {
	n, _ := ctx.Value(noticeCtxKey{}).(string)
	return n
}

type csrfCtxKey struct{}

// WithCSRFToken attaches the request's double-submit token at the render seam.
// Forms include it so clients that legitimately lack Origin and Fetch Metadata
// can still prove they loaded the form from this server.
func WithCSRFToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, csrfCtxKey{}, token)
}

// CSRFToken returns the token for the current rendered request.
func CSRFToken(ctx context.Context) string {
	token, _ := ctx.Value(csrfCtxKey{}).(string)
	return token
}

// Static holds the embedded static assets (htmx, stylesheet) served under
// /static/.
//
//go:embed static
var Static embed.FS

// Page is the shared layout context for every rendered page.
type Page struct {
	Title    string
	Username string
	IsAdmin  bool
	LoggedIn bool
	// Active is the sidebar nav key for the current page ("overview",
	// "sessions", "projects", "account"), so the shell can mark the
	// current section. Empty leaves no item active.
	Active string
	// OverviewPublic reports whether the signed-in user has published their own
	// usage overview, which drives the account page's Publicity controls and the
	// "Public" badge on the signed-in overview. The share link is /u/<username>, so
	// Username (already on the page) is all the badge and account section need to
	// build it. This is populated from the same UserByID lookup that fills Username,
	// so reading it costs no extra query.
	OverviewPublic bool
}

// OGMeta carries the Open Graph and Twitter card metadata a public page emits so a
// shared link unfurls with a title, description, and preview image. The zero value
// emits only the basic title tags; Image (an absolute URL) switches the Twitter
// card to the large-image form and adds og:image. Description and URL are optional.
type OGMeta struct {
	Title       string
	Description string
	// Image is the absolute URL of the preview card. Open Graph requires an absolute
	// URL here, so the handler builds it from the request origin; empty omits it.
	Image string
	// URL is the absolute canonical URL of the page; empty omits og:url.
	URL string
}

// plural returns the "s" suffix for a count, so a label reads "1 session" but
// "2 sessions" without each call site repeating the conditional.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// navClass returns the sidebar link's class, adding "active" when its key is the
// page's current section.
func navClass(key, active string) string {
	if key == active {
		return "nav active"
	}
	return "nav"
}

// RoleClass maps a message role to a CSS class for styling.
func RoleClass(role string) string {
	switch role {
	case "user":
		return "msg-user"
	case "assistant":
		return "msg-assistant"
	case "context":
		return "msg-context"
	default:
		return "msg-other"
	}
}
