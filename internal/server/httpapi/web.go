package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/a-h/templ"
	"github.com/jssblck/akari/internal/server/ogimage"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/web"
)

// requireReadHTML guards the server-rendered UI. Reading the UI needs a
// full-scope credential: a browser session in practice, though a full-scope API
// token reads the same surface its owner can (ingest-only tokens are rejected).
// Unauthenticated requests are redirected to the login page, not handed a JSON
// error.
func (s *Server) requireReadHTML(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.resolve(r)
		if !ok || p.Scope != scopeFull {
			http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		next(w, s.withPrincipal(r, p))
	}
}

// pageFor builds the shared layout context from the authenticated principal.
func (s *Server) pageFor(r *http.Request, title string) web.Page {
	pg := web.Page{Title: title}
	p, ok := principalFrom(r.Context())
	if !ok {
		return pg
	}
	pg.LoggedIn = true
	if u, err := s.Store.UserByID(r.Context(), p.UserID); err == nil {
		pg.Username = u.Username
		pg.IsAdmin = u.IsAdmin
		pg.OverviewPublic = u.OverviewPublic
	}
	return pg
}

// pageForNav is pageFor with the active sidebar section set, so the shell can
// mark the current nav item.
func (s *Server) pageForNav(r *http.Request, title, active string) web.Page {
	pg := s.pageFor(r, title)
	pg.Active = active
	return pg
}

// render writes a templ component with the given status. The status is committed
// before the body streams, so a mid-render failure cannot be remapped to a 500;
// it only truncates the response, which the browser surfaces as a broken page.
// Buffering the render to recover a clean 500 is deliberately not done: these
// pages are cheap to render and effectively never fail.
func render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	// Every HTML page renders through here, so resolving the viewer's timezone once
	// at this seam localizes every stamp and day heading (authed, public, and error
	// pages alike) without each handler having to thread a location through. The
	// helpers read it off the context via web.Loc, defaulting to UTC when the tz
	// cookie is absent.
	r = withLocation(r)
	// Likewise, draining any one-shot notice cookie here (rather than in pageFor)
	// means every render path clears it exactly once, regardless of which page the
	// action's redirect landed on; the authed layout reads it back via web.Notice.
	r = withNotice(w, r)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = c.Render(r.Context(), w)
}

// errorTitle is the page title an error status renders under. Deriving it here keeps
// every error page's tab title consistent with its status, instead of each call site
// restating the pairing.
func errorTitle(status int) string {
	switch status {
	case http.StatusNotFound:
		return "Not found"
	case http.StatusBadRequest:
		return "Bad request"
	case http.StatusForbidden:
		return "Forbidden"
	case http.StatusInternalServerError:
		return "Error"
	default:
		return http.StatusText(status)
	}
}

// renderError writes the signed-in error page. The status is named once and drives
// both the response code and the page body, so the two cannot disagree.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	render(w, r, status, web.ErrorPage(s.pageFor(r, errorTitle(status)), status, msg))
}

// renderErrorNav is renderError with the given nav tab kept highlighted, for errors
// raised inside one of the app's main surfaces.
func (s *Server) renderErrorNav(w http.ResponseWriter, r *http.Request, status int, nav, msg string) {
	render(w, r, status, web.ErrorPage(s.pageForNav(r, errorTitle(status), nav), status, msg))
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
	base := s.baseURL(r)
	// The meta copy derives from the ogimage package's canonical landing
	// constants (the same strings the /og.png card draws), so a copy edit
	// cannot leave the page's tags and its preview image saying different
	// things. The title lowercases the headline into the product register the
	// overview card's title uses ("<subject> · <what it is>").
	og := web.OGMeta{
		Title:       "akari · " + strings.ToLower(strings.TrimSuffix(ogimage.LandingHeadline, ".")),
		Description: ogimage.LandingSubline,
		URL:         base + "/",
		Image:       base + "/og.png",
	}
	// Resolve the viewer so a full-scope reader (a browser session in practice)
	// gets the signed-in topbar; a logged-out visitor or a non-full credential (an
	// ingest- or read-scope token pointed at the UI) gets the logged-out variant,
	// matching requireReadHTML's gate on the rest of the UI.
	var viewer web.Page
	if p, ok := s.resolve(r); ok && p.Scope == scopeFull {
		viewer = s.pageFor(s.withPrincipal(r, p), "akari")
	}
	render(w, r, http.StatusOK, web.LandingPage(og, viewer))
}

// handleOverview is the app's home surface at /overview: the audit verdict and its
// needs-attention shortlist over fleet-wide usage, bounded to the selected trailing
// window and users. The range selector and the per-user filter refetch this same
// handler and swap the audit-plus-usage wrapper (hx-select="#overview-usage"), so a
// plain load and an htmx swap render from one path; the window and selection ride the
// URL via ?range= and ?user=.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	rng := web.ParseRange(r.URL.Query().Get("range"))
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		s.renderErrorNav(w, r, http.StatusInternalServerError, "overview", "Could not load users.")
		return
	}
	selected := web.SelectedUserIDs(r.URL.Query()["user"], users)
	filter := store.AnalyticsFilter{Since: web.RangeSince(rng, time.Now()), UserIDs: selected}
	// Read the usage analytics and the audit verdict from one snapshot: the Spend tile shows
	// the analytics total with the audit's failed-run spend pulled out beneath it, so the two
	// must come from one MVCC view or the subfigure could disagree with the total it annotates.
	analytics, audit, err := s.Store.OverviewData(r.Context(), filter)
	if err != nil {
		s.renderErrorNav(w, r, http.StatusInternalServerError, "overview", "Could not load analytics.")
		return
	}
	setDashboardCache(w)
	render(w, r, http.StatusOK, web.OverviewPage(s.pageForNav(r, "Overview", "overview"), analytics, audit, rng, users, selected))
}

// handleInsights is the cross-cutting analytics surface at /insights: the quality and
// archetype distributions over the selected trailing window. Like the overview, the
// range selector refetches this same handler and swaps the insights section
// (hx-select="#insights"), so a plain load and an htmx swap render from one path; the
// window rides the URL via ?range=.
func (s *Server) handleInsights(w http.ResponseWriter, r *http.Request) {
	rng := web.ParseRange(r.URL.Query().Get("range"))
	// The snapshot is memoized per range for a short TTL (insights_cache.go): the
	// pipeline behind it is a dozen aggregate queries over the whole window, so a
	// map lookup here is the difference between an instant load and several seconds.
	// The Bucket names the trend grid's unit (day for short windows, week for long),
	// which switches on the trend computation inside Insights: the fleet page draws
	// time series, so it always asks for a grid, unlike the project quality band.
	start := time.Now()
	ins, err := s.insights.load(r.Context(), rng, start, func(ctx context.Context) (store.Insights, error) {
		return s.Store.Insights(ctx, store.AnalyticsFilter{
			Since:  web.RangeSince(rng, time.Now()),
			Bucket: web.TrendBucket(rng),
		})
	})
	if err != nil {
		s.renderErrorNav(w, r, http.StatusInternalServerError, "insights", "Could not load insights.")
		return
	}
	// Expose the server-side load time so a cold miss (the full parallel pipeline) versus a
	// warm cache hit (a map lookup) reads straight off the Server-Timing entry in devtools,
	// which is how the revamp's perf target stays visible to a future regression.
	w.Header().Set("Server-Timing", fmt.Sprintf("insights;dur=%.1f", float64(time.Since(start).Microseconds())/1000))
	setDashboardCache(w)
	ranges := web.RangeOptions("/insights", nil, rng)
	render(w, r, http.StatusOK, web.InsightsPage(s.pageForNav(r, "Insights", "insights"), ins, rng, ranges))
}

// dashboardCacheMaxAge lets a browser reuse a dashboard page for a few seconds, so
// back/forward navigation and the range selector's hx-select refetch do not re-run
// the query pipeline. It is private (these pages carry the viewer's own sidebar and
// windowed figures) and short enough that a reader who pauses sees fresh numbers.
// The server-side insights cache absorbs the cross-viewer and post-expiry load
// behind it. The sessions feed is deliberately excluded: it is already a single
// indexed query and changes on every ingest, so a stale feed would mislead.
const dashboardCacheMaxAge = 30

func setDashboardCache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, max-age="+strconv.Itoa(dashboardCacheMaxAge))
}

// handleNotFound is the catch-all for a GET whose path no route claims: a typed or
// stale URL, or a guessed pretty project path (projects are keyed by numeric id, so
// /projects/github.com/owner/repo lands here). It renders the styled error page in
// the viewer's shell rather than net/http's bare "404 page not found" text, with a
// way back into the app. It gates nothing: an error page shows no parsed data, and a
// logged-out visitor should get the public 404 rather than a login bounce.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if p, ok := s.resolve(r); ok && p.Scope == scopeFull {
		render(w, r, http.StatusNotFound, web.ErrorPage(s.pageFor(s.withPrincipal(r, p), "Not found"), http.StatusNotFound, "That page does not exist."))
		return
	}
	render(w, r, http.StatusNotFound, web.PublicErrorPage(http.StatusNotFound, "That page does not exist."))
}

// handleProjectsIndex is the projects table at /projects (moved off the root when
// Overview became the app's home surface).
func (s *Server) handleProjectsIndex(w http.ResponseWriter, r *http.Request) {
	projects, err := s.Store.ListProjects(r.Context())
	if err != nil {
		s.renderErrorNav(w, r, http.StatusInternalServerError, "projects", "Could not load projects.")
		return
	}
	// The index splits git-remote repositories from local (standalone/orphaned)
	// folders into two sections: a repository is the audit unit a reader scans for
	// first, a local folder the looser catch-all beneath it. The store returns both
	// kinds in one activity-ordered list; partition it here so the template renders
	// each section in that order.
	var remotes, locals []store.ProjectSummary
	for _, pr := range projects {
		if web.IsLocalKind(pr.Kind) {
			locals = append(locals, pr)
		} else {
			remotes = append(remotes, pr)
		}
	}
	spark, err := s.Store.ProjectSparklines(r.Context(), 30)
	if err != nil {
		s.renderErrorNav(w, r, http.StatusInternalServerError, "projects", "Could not load analytics.")
		return
	}
	setDashboardCache(w)
	render(w, r, http.StatusOK, web.ProjectsPage(s.pageForNav(r, "Projects", "projects"), remotes, locals, spark))
}

// handleSessions is the global, faceted session list across every project. An
// htmx request swaps only the list; a normal load renders the page with its
// filter rail.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := store.SessionFilter{
		Agent:    strings.TrimSpace(q.Get("agent")),
		Machine:  strings.TrimSpace(q.Get("machine")),
		Username: strings.TrimSpace(q.Get("user")),
	}
	// A present-but-malformed project filter is a bad request, not a silent
	// fall-through to the unfiltered list (which would mislead the caller).
	if v := strings.TrimSpace(q.Get("project")); v != "" {
		pid, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			s.renderErrorNav(w, r, http.StatusBadRequest, "sessions", "Invalid project filter.")
			return
		}
		filter.ProjectID = pid
	}
	// Content search: a trimmed, length-capped query restricts the feed to sessions
	// with a matching message and drives the per-row snippet. An empty query behaves
	// exactly as before. The cap bounds the ILIKE pattern to a sane length so a
	// pathological query cannot force a huge scan.
	if v := strings.TrimSpace(q.Get("q")); v != "" {
		if len(v) > maxSearchQueryLen {
			// Cut on a rune boundary: a byte-mid-rune truncation would hand Postgres
			// invalid UTF-8, which it rejects with an encoding error.
			cut := maxSearchQueryLen
			for cut > 0 && !utf8.RuneStart(v[cut]) {
				cut--
			}
			v = v[:cut]
		}
		filter.Query = v
	}
	// Grade and outcome arrive from an Insights drill-through link. Each is
	// whitelist-validated: a present-but-unknown value is a bad request, not a silent
	// fall-through to the unfiltered list, matching the project-filter precedent above.
	if v := strings.TrimSpace(q.Get("grade")); v != "" {
		if !web.IsGrade(v) {
			s.renderErrorNav(w, r, http.StatusBadRequest, "sessions", "Invalid grade filter.")
			return
		}
		filter.Grade = v
	}
	if v := strings.TrimSpace(q.Get("outcome")); v != "" {
		if !web.IsOutcome(v) {
			s.renderErrorNav(w, r, http.StatusBadRequest, "sessions", "Invalid outcome filter.")
			return
		}
		filter.Outcome = v
	}
	// The feed accepts a ?range drill-down bound (7d/30d/90d/year, the RangeBounds keys), so a
	// bar or People link from the windowed Insights/project analytics opens a feed scoped to the
	// same trailing window its count described. Unlike the analytics surfaces, the feed's default
	// is all-history, not the trailing year: web.RangeBounds is the whitelist, so an absent, "all",
	// or hand-typed junk value reads as unbounded rather than falling to ParseRange's trailing-year
	// default. Only an explicitly bounded key sets the window. The window binds s.started_at:
	// ListAllSessions scopes Since to started_at, the column the Insights and People panels window
	// their cohorts by, so a drill-through from a panel bar opens exactly the sessions that bar
	// counted rather than a session that merely re-activated inside the window. filter.Range carries
	// the key so the URL round-trips ?range= and the active-range chip can label and clear the window.
	if rng := strings.TrimSpace(q.Get("range")); web.RangeBounds(rng) {
		filter.Range = rng
		filter.Since = web.RangeSince(rng, time.Now())
	}
	// Empty sessions (message_count = 0) are hidden by default; empty=1 shows them.
	filter.IncludeEmpty = q.Get("empty") == "1"
	// Subagent sessions are hidden by default so the feed reads as top-level work;
	// subagents=1 folds them back in (they stay reachable from each parent's page).
	filter.IncludeSubagents = q.Get("subagents") == "1"
	// spanned=1 narrows to sessions with a measured span, the concurrency panel's cohort;
	// it arrives only on the busiest-user drill so that feed matches what the panel swept.
	filter.RequireSpan = q.Get("spanned") == "1"
	// The feed reads one fixed-size page (DefaultSessionLimit). "Show more" no longer grows
	// this: it passes a keyset cursor (after) and appends the next page of the same size, so
	// depth is unbounded and every page's read cost stays flat.
	filter.Limit = web.DefaultSessionLimit
	// Click-to-sort: an unknown sort key falls back to the default order rather
	// than erroring, so a stale or tampered link still renders the feed. The
	// direction defaults to descending; the header links always carry an explicit
	// dir, so this only catches hand-edited URLs.
	filter.Sort = store.DefaultSort
	if v := strings.TrimSpace(q.Get("sort")); store.IsSortKey(v) {
		filter.Sort = v
	}
	filter.Desc = q.Get("dir") != "asc"
	// Keyset cursor: "Show more" carries the last row the reader saw as ?after=<id>, so the
	// store resumes strictly after it in the current order rather than re-reading the page
	// under a bigger limit. A malformed or non-positive value is no cursor (the first page).
	// The store applies it only to the keyset-sortable orders and ignores it otherwise.
	if v := strings.TrimSpace(q.Get("after")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			filter.After = n
		}
	}
	// after_day is the day-bucket key of the cursor row, carried only in the day-grouped
	// default order so the appended page can suppress a heading that repeats the day the
	// prior page ended on. count is the running total already shown, so the appended footer
	// reports the cumulative "Showing N" without counting the corpus. carriedMax is the
	// feed's token-bar denominator the first page established, so an appended page scales its
	// bars against the same reference rather than re-normalizing to its own page maximum. All
	// three are honored only on an actual continuation (a cursor is set), so a stray param on
	// a first-page URL cannot inflate the count or skew the bars.
	afterDay := strings.TrimSpace(q.Get("after_day"))
	priorCount := 0
	var carriedMax int64
	if filter.After > 0 {
		// av is the cursor row's sort value as the page rendered it, so the resume boundary
		// stays fixed at what the reader saw even if that row's own column later moves (see
		// store.SessionFilter.AfterVal). Validate it against the active sort's type so a
		// hand-tampered value is dropped (falling back to the id-only cursor) rather than
		// failing the SQL cast; a valid value only ever comes from our own ShowMorePath.
		if v := strings.TrimSpace(q.Get("av")); v != "" && web.ValidKeysetValue(filter.Sort, v) {
			filter.AfterVal = v
		}
		if v := strings.TrimSpace(q.Get("count")); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				priorCount = n
			}
		}
		if v := strings.TrimSpace(q.Get("maxtok")); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				carriedMax = n
			}
		}
	}
	// The day-continuation and the next cursor's day key are resolved against the viewer's
	// zone, so attach it now rather than at render time (render's own withLocation is
	// idempotent): the handler-side FeedDayKey below must bucket a row the same way the
	// template does, or a page boundary could drop or double a day heading.
	r = withLocation(r)
	// The list fetches limit+1 rows and reports hasMore, so the footer learns whether a
	// next page exists without a count(*) over the whole matching history: the render
	// cost stays linear in the page, not the corpus.
	rows, hasMore, err := s.Store.ListAllSessions(r.Context(), filter)
	if err != nil {
		s.renderErrorNav(w, r, http.StatusInternalServerError, "sessions", "Could not load sessions.")
		return
	}
	// The empty-hidden toggle needs only whether any empty session exists in scope, not
	// how many: that yes/no is a bounded EXISTS probe rather than the O(total) aggregate
	// the old count carried. It probes regardless of the current IncludeEmpty state:
	// HasEmptySessions forces empties into scope to answer "are there any here", so even
	// when they are being shown the toggle appears only when hiding them would actually
	// change the feed. A ?empty=1 over a scope with no empties thus shows no toggle,
	// rather than a "showing empty · hide" that would hide nothing.
	hasEmpty, err := s.Store.HasEmptySessions(r.Context(), filter)
	if err != nil {
		s.renderErrorNav(w, r, http.StatusInternalServerError, "sessions", "Could not load sessions.")
		return
	}
	// lastDayKey is this page's last row's day bucket, carried into the next "Show more"
	// cursor so the following page continues the same day without reprinting its heading.
	// It applies only to the day-grouped default order; the flat sorts carry no day key.
	lastDayKey := ""
	if web.FeedIsGrouped(filter) && len(rows) > 0 {
		lastDayKey = web.FeedDayKey(r.Context(), rows[len(rows)-1].LastActiveAt)
	}
	// The token-bar denominator is the first page's largest session, carried forward on each
	// "Show more" (carriedMax) so a bar's width means the same magnitude across pages. When no
	// cursor carries one (the first page, or a hand-edited link), fall back to this page's own
	// maximum so bars still render.
	maxTok := carriedMax
	if maxTok <= 0 {
		maxTok = web.FeedMaxTokens(rows)
	}
	footer := web.BuildSessionFooter(filter, rows, priorCount, hasMore, hasEmpty, lastDayKey, maxTok)
	if r.Header.Get("HX-Request") == "true" {
		// "Show more" appends the next keyset page in place (FeedAppend replaces #feed-more
		// with the new rows plus a fresh tail). A bare htmx request with no cursor (not
		// something the UI issues) still degrades to the full list body.
		if filter.After > 0 {
			render(w, r, http.StatusOK, web.FeedAppend(rows, filter, footer, afterDay))
		} else {
			render(w, r, http.StatusOK, web.GlobalSessionList(rows, filter, footer))
		}
		return
	}
	facets, err := s.Store.GlobalFacets(r.Context())
	if err != nil {
		s.renderErrorNav(w, r, http.StatusInternalServerError, "sessions", "Could not load filters.")
		return
	}
	render(w, r, http.StatusOK, web.SessionsPage(s.pageForNav(r, "Sessions", "sessions"), rows, facets, filter, footer))
}

// maxSearchQueryLen caps the content-search string before it becomes an ILIKE
// pattern, so a pasted multi-kilobyte query cannot drive a pathological scan. It is
// generous for any real search term.
const maxSearchQueryLen = 200

func (s *Server) handleProjectPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "Project not found.")
		return
	}
	proj, err := s.Store.Project(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "Project not found.")
		return
	}
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Could not load project.")
		return
	}

	// The same trailing window bounds both the usage panel and the session list, so
	// the two read the same range; the heatmap's selector and the filter form both
	// carry it on ?range.
	rng := web.ParseRange(r.URL.Query().Get("range"))
	since := web.RangeSince(rng, time.Now())
	filter := store.SessionFilter{
		ProjectID: id,
		Agent:     strings.TrimSpace(r.URL.Query().Get("agent")),
		Machine:   strings.TrimSpace(r.URL.Query().Get("machine")),
		Username:  strings.TrimSpace(r.URL.Query().Get("user")),
		Since:     since,
		// The per-project table is windowed by dated usage and reconciles with the
		// usage panel; the empty-hiding is a global-feed affordance only, so keep every
		// session here regardless of message count.
		IncludeEmpty: true,
	}
	// The table draws from the same windowed usage base as the panel (WindowSessionPage,
	// not the lifetime-rollup ListSessions), so each row's tokens and cost are its
	// in-window share and the visible rows sum to the panel headline rather than
	// overcounting sessions whose usage predates the window. Past the row cap the page
	// carries a remainder aggregate so the table still reconciles with the headline.
	page, err := s.Store.WindowSessionPage(r.Context(), filter)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Could not load sessions.")
		return
	}

	facets, err := s.Store.SessionFacets(r.Context(), id)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Could not load filters.")
		return
	}
	// The usage panel scopes to the same agent/user/machine the session table does, so
	// the headline and the rows reconcile under a filter rather than the panel staying
	// project-wide while the rows narrow. The same filter values feed both: the panel
	// matches the username through the analytics base (an unknown name scopes to
	// nothing, matching the empty table) rather than a separate lookup whose error
	// would have to be invented away.
	af := store.AnalyticsFilter{
		ProjectID: id, Since: since, Username: filter.Username, Agent: filter.Agent, Machine: filter.Machine,
	}
	analytics, err := s.Store.Analytics(r.Context(), af)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Could not load analytics.")
		return
	}
	// The quality band draws from the same scope as the usage panel and the table (the same
	// AnalyticsFilter: project, window, and any active user/agent/machine narrowing), so the
	// grades, outcomes, tools, and churn describe exactly the sessions the rows below list
	// rather than a project-wide read that would drift from the filtered table.
	//
	// Two windows meet here on purpose. Insights counts sessions by started_at falling in the
	// trailing window; the usage panel and the session table above window on dated usage_events.
	// The gap is intentional and not reconciled: a quality verdict is a per-session fact keyed to
	// when the session ran, so the band follows the Insights (started_at) convention, while spend
	// is per-usage-event and windows on the event dates. Forcing one onto the other would misdate
	// whichever it borrows, so the band's section head names its window ("sessions that started in
	// this window") instead. See projectQuality for the matching caption.
	insights, err := s.Store.Insights(r.Context(), af)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Could not load quality signals.")
		return
	}
	wf := web.Facets{Agents: facets.Agents, Machines: facets.Machines, Users: facets.Users}
	render(w, r, http.StatusOK, web.ProjectPage(s.pageForNav(r, proj.RemoteKey, "projects"), proj, page.Sessions, page.Remainder, wf, filter, analytics, insights, rng))
}

// sessionView loads everything the session page (and its live body fragment) renders:
// detail, the bounded outline rows, the windowed transcript tail, tool metadata and
// attachments grouped by message, subagents with their verdicts, the whole-work-item
// rollup, the serving models, and the header stats. Each transcript message carries its
// own per-turn usage (Message.Usage) and duplicate-prompt verdict
// (Message.DuplicatePrompt), folded in the windowed read itself, so the transcript's
// context/cost stamps and repeat badges need no second session-sized structure beside
// the window the page renders.
//
// The transcript is the session's tail, not the whole session: an unbounded render is
// what froze the tab on long sessions (P-2). Everything comes from one store snapshot
// (store.SessionSnapshotByID), so the window, the shape surfaces, and the audit rows
// can never mix projections across a mid-request rebuild.
func (s *Server) sessionView(r *http.Request, id int64) (web.SessionView, error) {
	snap, err := s.Store.SessionSnapshotByID(r.Context(), id)
	if err != nil {
		return web.SessionView{}, err
	}
	return sessionViewFrom(snap), nil
}

// sessionViewFrom builds the web view from one store snapshot. Pure assembly: every
// row it maps was read in the snapshot's transaction, so nothing here can straddle a
// rebuild.
func sessionViewFrom(snap store.SessionSnapshot) web.SessionView {
	v := web.SessionView{
		Detail:    snap.Audit.Detail,
		Subagents: snap.Audit.Subagents,
		Tree:      snap.Audit.Tree,
		Models:    snap.Audit.Models,
		Outline:   snap.Outline,
		Tools:     web.ToolsByOrdinal(snap.Tools),
		DupIDs:    snap.DupIDs,
		Header:    sessionHeaderStats(snap.Audit.Detail, snap.Audit.Signals, snap.Audit.Fallbacks),
	}
	v.SetPage(snap.Page)
	return v
}

// sessionHeaderStats builds the derived stat-tile inputs the session instrument header
// renders: the session's all-usage cache effectiveness, its stored quality signals, and
// the header tile's capped fallback list. Pure derivation over rows the caller already
// holds: the session page passes them from one store snapshot (SessionAudit), so the
// tiles can never mix projections, and the public page passes its own pool reads.
//
// The Cache tile comes straight off the SessionDetail rollups (the token classes plus
// the parse-time cache-savings fold). That is the point of the rollup: the live body
// re-renders on every SSE update, and reading the tile from the row the caller already
// holds keeps a long session's K refreshes linear rather than rescanning its K usage
// rows each time.
func sessionHeaderStats(d store.SessionDetail, sig store.SessionSignals, fallbacks []store.ModelFallback) web.HeaderStats {
	cache := store.CacheStats{
		Input:             d.TotalInput,
		Output:            d.TotalOutput,
		CacheRead:         d.TotalCacheRead,
		CacheWrite:        d.TotalCacheWrite,
		SavingsUSD:        d.TotalCacheSavingsUSD,
		SavingsIncomplete: d.CacheSavingsIncomplete,
	}
	// The observed-thinking band is an absolute cut on the token scale, carried whole by the
	// stored signals row, so the readout reads straight from the row with no extra query: the
	// band, the tail and peak per-turn token volumes, and the coverage all come from the
	// figures the settle pass already derived. An unmeasured session (no assistant turns)
	// leaves the readout empty so the header shows no thinking block.
	thinking := web.ThinkingReadout{}
	if sig.HasThinkingMeasure() {
		thinking = web.ThinkingReadout{
			Measured:   true,
			Bucket:     sig.ThinkingBucket(),
			Turns:      *sig.ThinkingTurns,
			TailTokens: *sig.ThinkingTailTokens,
			PeakTokens: *sig.ThinkingPeakTokens,
			Coverage:   sig.ThinkingCoverage(),
		}
	}
	return web.HeaderStats{Cache: cache, Signals: sig, Fallbacks: fallbacks, Thinking: thinking}
}

func (s *Server) handleSessionPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusNotFound, "Session not found.")
		return
	}
	v, err := s.sessionView(r, id)
	if errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "Session not found.")
		return
	}
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Could not load session.")
		return
	}
	title := web.SessionPageTitle(v.Detail)
	p, _ := principalFrom(r.Context())
	owner := p.UserID == v.Detail.OwnerID
	render(w, r, http.StatusOK, web.SessionPage(s.pageForNav(r, title, "sessions"), v, true, owner))
}

// handleSessionBody serves the live body's fragments. Three shapes, chosen by query
// param, all stateless (the client names its own position, per the P-2 render map):
//
//   - ?after=N: the turns past the last ordinal the client rendered, for the SSE
//     append, plus out-of-band swaps for the instruments and subagents. When the
//     client is too far behind for one append (the window read hit its cap, or its
//     ordinal has run past the projection after an epoch rebuild reshaped it), the
//     response retargets to #session-body and re-renders the windowed body whole, so
//     the client can never assemble a transcript with a hidden gap.
//   - ?before=N: the window of turns preceding ordinal N, for "Show earlier".
//   - bare: the whole windowed body plus the out-of-band ribbon and outline
//     (SessionResync), the full-refresh fallback.
func (s *Server) handleSessionBody(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if a := q.Get("after"); a != "" {
		s.serveSessionAppend(w, r, id, a)
		return
	}
	if b := q.Get("before"); b != "" {
		s.serveSessionEarlier(w, r, id, b)
		return
	}
	v, err := s.sessionView(r, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		// A transient backend failure is a 500, not a "session not found": the live
		// body fragment must not report a database hiccup as a missing session.
		http.Error(w, "Could not load session.", http.StatusInternalServerError)
		return
	}
	// The full-refresh fallback arrives via htmx (app.js swaps it into #session-body),
	// so the resync's out-of-band ribbon and outline land too: a transcript rendering
	// its first rows populates the shape surfaces in the same swap.
	render(w, r, http.StatusOK, web.SessionResync(v))
}

// serveSessionAppend answers ?after=N: the incremental SSE path.
func (s *Server) serveSessionAppend(w http.ResponseWriter, r *http.Request, id int64, arg string) {
	after, err := strconv.Atoi(arg)
	if err != nil {
		http.Error(w, "bad after cursor", http.StatusBadRequest)
		return
	}
	snap, err := s.Store.SessionAppendByID(r.Context(), id, after)
	if err != nil {
		s.fragmentError(w, err)
		return
	}
	// An append can only extend a transcript the client already holds a true prefix
	// of. Two cases break that: the read hit its cap (More: too many new rows for one
	// fragment) and a cursor naming an ordinal the projection no longer has (an epoch
	// rebuild reshaped or emptied the transcript under the open tab, so the DOM no
	// longer matches). The seed proves the cursor: it reads backward from `after`
	// inclusive, so its last row carries exactly `after` whenever that row exists. A
	// client only sends ?after= for rows it actually rendered, so any invalid cursor,
	// including one over a projection now empty, re-renders the windowed body whole;
	// htmx honors the retarget headers.
	cursorValid := len(snap.Page.Seed) > 0 && snap.Page.Seed[len(snap.Page.Seed)-1].Ordinal == after
	if snap.Page.More || !cursorValid {
		v, err := s.sessionView(r, id)
		if err != nil {
			s.fragmentError(w, err)
			return
		}
		w.Header().Set("HX-Retarget", "#session-body")
		w.Header().Set("HX-Reswap", "innerHTML")
		render(w, r, http.StatusOK, web.SessionResync(v))
		return
	}
	render(w, r, http.StatusOK, web.TranscriptAppend(sessionViewFrom(snap)))
}

// serveSessionEarlier answers ?before=N: the "Show earlier" path. The fragment renders
// only settled rows, whose page carries its own tools, attachments, and fallback
// notices from one store snapshot; there are no out-of-band refreshes, so the detail
// and the page are all it needs.
func (s *Server) serveSessionEarlier(w http.ResponseWriter, r *http.Request, id int64, arg string) {
	before, err := strconv.Atoi(arg)
	if err != nil {
		http.Error(w, "bad before cursor", http.StatusBadRequest)
		return
	}
	d, page, err := s.Store.SessionEarlierByID(r.Context(), id, before)
	if err != nil {
		s.fragmentError(w, err)
		return
	}
	v := web.SessionView{Detail: d}
	v.SetPage(page)
	render(w, r, http.StatusOK, web.TranscriptEarlier(v))
}

// fragmentError maps a fragment load failure to its status: a vanished session is 404,
// anything else a 500 (never "not found" for a database hiccup).
func (s *Server) fragmentError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "Session not found.", http.StatusNotFound)
		return
	}
	http.Error(w, "Could not load session.", http.StatusInternalServerError)
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
	serveSSE(w, r, ch, func(struct{}) string { return "event: update\ndata: 1\n\n" }, nil)
}

// safeNext bounds a post-login redirect target to a local path, so a crafted
// next= cannot bounce the user to another origin.
// overviewPath is the app's home surface: where a fresh sign-in lands and the
// fallback for a login with no saved destination. The root "/" is the public
// homepage now, so post-auth flows aim here rather than dropping the user back on
// the marketing page.
const overviewPath = "/overview"

// safeNext sanitizes a post-login redirect target, rejecting anything that is not
// a same-origin absolute path (so a crafted next cannot bounce the user off-site).
// An empty or rejected value falls back to the app home rather than the public
// root, so a bare visit to /login still lands in the app after signing in.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return overviewPath
	}
	return next
}

// setFlash stores a one-shot value in a short-lived cookie. These cookies carry
// freshly minted secrets, so they honor the same Secure setting as the session
// cookie to avoid leaking a secret over plain HTTP on an HTTPS deployment.
func (s *Server) setFlash(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    url.QueryEscape(value),
		Path:     "/account",
		HttpOnly: true,
		Secure:   s.Cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   60,
	})
}

// readFlash reads and immediately clears a flash cookie.
func readFlash(w http.ResponseWriter, r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/account", MaxAge: -1})
	v, err := url.QueryUnescape(c.Value)
	if err != nil {
		return ""
	}
	return v
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
