package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/a-h/templ"
	"github.com/jssblck/akari/internal/server/auth"
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

// handleRoot serves the site root at /. A signed-in reader (full-scope
// credential, a browser session in practice) gets the overview, the app's
// landing surface, gated during a reparse like the rest of the parsed UI. A
// logged-out visitor gets the marketing landing page explaining what akari is,
// rather than an immediate bounce to the login form. A non-full credential (an
// ingest- or read-scope token pointed at the browser UI) is treated as
// logged-out here, matching requireReadHTML's gate on the rest of the UI.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	p, ok := s.resolve(r)
	if !ok || p.Scope != scopeFull {
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
		render(w, r, http.StatusOK, web.LandingPage(og))
		return
	}
	s.gateParsed(s.handleOverview)(w, s.withPrincipal(r, p))
}

// handleOverview is the landing surface at /: fleet-wide usage bounded to the
// selected trailing window. The range selector refetches this same handler and
// swaps the usage panel (hx-select="#usage"), so a plain load and an htmx swap
// render from one path; the window also rides the URL via ?range=.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		render(w, r, http.StatusNotFound, web.ErrorPage(s.pageForNav(r, "Not found", ""), http.StatusNotFound, "Page not found."))
		return
	}
	rng := web.ParseRange(r.URL.Query().Get("range"))
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageForNav(r, "Error", "overview"), http.StatusInternalServerError, "Could not load users."))
		return
	}
	selected := web.SelectedUserIDs(r.URL.Query()["user"], users)
	analytics, err := s.Store.Analytics(r.Context(), store.AnalyticsFilter{Since: web.RangeSince(rng, time.Now()), UserIDs: selected})
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageForNav(r, "Error", "overview"), http.StatusInternalServerError, "Could not load analytics."))
		return
	}
	render(w, r, http.StatusOK, web.OverviewPage(s.pageForNav(r, "Overview", "overview"), analytics, rng, users, selected))
}

// handleInsights is the cross-cutting analytics surface at /insights: the quality and
// archetype distributions over the selected trailing window. Like the overview, the
// range selector refetches this same handler and swaps the insights section
// (hx-select="#insights"), so a plain load and an htmx swap render from one path; the
// window rides the URL via ?range=.
func (s *Server) handleInsights(w http.ResponseWriter, r *http.Request) {
	rng := web.ParseRange(r.URL.Query().Get("range"))
	ins, err := s.Store.Insights(r.Context(), store.AnalyticsFilter{Since: web.RangeSince(rng, time.Now())})
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageForNav(r, "Error", "insights"), http.StatusInternalServerError, "Could not load insights."))
		return
	}
	ranges := web.RangeOptions("/insights", nil, rng)
	render(w, r, http.StatusOK, web.InsightsPage(s.pageForNav(r, "Insights", "insights"), ins, rng, ranges))
}

// handleProjectsIndex is the projects table (moved off the root to /projects when
// Overview became the landing surface).
func (s *Server) handleProjectsIndex(w http.ResponseWriter, r *http.Request) {
	projects, err := s.Store.ListProjects(r.Context())
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageForNav(r, "Error", "projects"), http.StatusInternalServerError, "Could not load projects."))
		return
	}
	// The index lists git-remote projects only. Local (standalone/orphaned)
	// folders reach the reader through the Sessions filter rail, so they are kept
	// off this surface rather than crowding it with a second table.
	var remotes []store.ProjectSummary
	for _, pr := range projects {
		if !web.IsLocalKind(pr.Kind) {
			remotes = append(remotes, pr)
		}
	}
	spark, err := s.Store.ProjectSparklines(r.Context(), 30)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageForNav(r, "Error", "projects"), http.StatusInternalServerError, "Could not load analytics."))
		return
	}
	render(w, r, http.StatusOK, web.ProjectsPage(s.pageForNav(r, "Projects", "projects"), remotes, spark))
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
			render(w, r, http.StatusBadRequest, web.ErrorPage(s.pageForNav(r, "Bad request", "sessions"), http.StatusBadRequest, "Invalid project filter."))
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
			render(w, r, http.StatusBadRequest, web.ErrorPage(s.pageForNav(r, "Bad request", "sessions"), http.StatusBadRequest, "Invalid grade filter."))
			return
		}
		filter.Grade = v
	}
	if v := strings.TrimSpace(q.Get("outcome")); v != "" {
		if !web.IsOutcome(v) {
			render(w, r, http.StatusBadRequest, web.ErrorPage(s.pageForNav(r, "Bad request", "sessions"), http.StatusBadRequest, "Invalid outcome filter."))
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
	// spanned=1 narrows to sessions with a measured span, the concurrency panel's cohort;
	// it arrives only on the busiest-user drill so that feed matches what the panel swept.
	filter.RequireSpan = q.Get("spanned") == "1"
	// The paging limit rides the URL, doubled by "Show more" and clamped to the
	// store's window. An absent or malformed value is the default page.
	filter.Limit = web.DefaultSessionLimit
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > web.MaxSessionLimit {
				n = web.MaxSessionLimit
			}
			filter.Limit = n
		}
	}
	// Click-to-sort: an unknown sort key falls back to the default order rather
	// than erroring, so a stale or tampered link still renders the feed. The
	// direction defaults to descending; the header links always carry an explicit
	// dir, so this only catches hand-edited URLs.
	filter.Sort = store.DefaultSort
	if v := strings.TrimSpace(q.Get("sort")); store.IsSortKey(v) {
		filter.Sort = v
	}
	filter.Desc = q.Get("dir") != "asc"
	// The list fetches limit+1 rows and reports hasMore, so the footer learns whether a
	// next page exists without a count(*) over the whole matching history: the render
	// cost stays linear in the page, not the corpus.
	rows, hasMore, err := s.Store.ListAllSessions(r.Context(), filter)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageForNav(r, "Error", "sessions"), http.StatusInternalServerError, "Could not load sessions."))
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
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageForNav(r, "Error", "sessions"), http.StatusInternalServerError, "Could not load sessions."))
		return
	}
	footer := web.BuildSessionFooter(filter, len(rows), hasMore, hasEmpty)
	if r.Header.Get("HX-Request") == "true" {
		render(w, r, http.StatusOK, web.GlobalSessionList(rows, filter, footer))
		return
	}
	facets, err := s.Store.GlobalFacets(r.Context())
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageForNav(r, "Error", "sessions"), http.StatusInternalServerError, "Could not load filters."))
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
		render(w, r, http.StatusNotFound, web.ErrorPage(s.pageFor(r, "Not found"), http.StatusNotFound, "Project not found."))
		return
	}
	proj, err := s.Store.Project(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		render(w, r, http.StatusNotFound, web.ErrorPage(s.pageFor(r, "Not found"), http.StatusNotFound, "Project not found."))
		return
	}
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not load project."))
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
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not load sessions."))
		return
	}

	facets, err := s.Store.SessionFacets(r.Context(), id)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not load filters."))
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
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not load analytics."))
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
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not load quality signals."))
		return
	}
	wf := web.Facets{Agents: facets.Agents, Machines: facets.Machines, Users: facets.Users}
	render(w, r, http.StatusOK, web.ProjectPage(s.pageForNav(r, proj.RemoteKey, "projects"), proj, page.Sessions, page.Remainder, wf, filter, analytics, insights, rng))
}

// sessionView loads everything the session page (and its live body fragment)
// needs: detail, transcript, tool metadata and attachments grouped by message, and
// subagents. Each message carries its own per-turn usage (Message.Usage) and duplicate-prompt
// verdict (Message.DuplicatePrompt), folded in the Messages read itself, so the transcript's
// context/cost stamps and repeat badges need no second session-sized structure beside the message
// slice the page already renders.
func (s *Server) sessionView(r *http.Request, id int64) (store.SessionDetail, []store.Message, map[int][]store.ToolCallView, map[int][]store.AttachmentView, []store.SessionSummary, error) {
	d, err := s.Store.SessionDetailByID(r.Context(), id)
	if err != nil {
		return d, nil, nil, nil, nil, err
	}
	msgs, err := s.Store.Messages(r.Context(), id)
	if err != nil {
		return d, nil, nil, nil, nil, err
	}
	tools, err := s.Store.ToolCalls(r.Context(), id)
	if err != nil {
		return d, nil, nil, nil, nil, err
	}
	atts, err := s.Store.Attachments(r.Context(), id)
	if err != nil {
		return d, nil, nil, nil, nil, err
	}
	subs, err := s.Store.Subagents(r.Context(), id)
	if err != nil {
		return d, nil, nil, nil, nil, err
	}
	return d, msgs, web.ToolsByOrdinal(tools), web.AttachmentsByOrdinal(atts), subs, nil
}

// sessionHeaderStats builds the derived stat-tile inputs the session instrument header
// renders: the session's all-usage cache effectiveness and its stored quality signals.
// Both session-page handlers and the live body fragment share it, so the header reads the
// same way on first load and on every SSE refresh.
//
// The Cache tile comes straight off the already-loaded SessionDetail rollups (the token
// classes plus the parse-time cache-savings fold), so it costs nothing here. That is the
// point of the rollup: the live body re-renders on every SSE update, and reading the tile
// from the row the caller already holds keeps a long session's K refreshes linear rather
// than rescanning its K usage rows each time. Only the stored signals still need a read.
func (s *Server) sessionHeaderStats(ctx context.Context, d store.SessionDetail) (web.HeaderStats, error) {
	cache := store.CacheStats{
		Input:             d.TotalInput,
		Output:            d.TotalOutput,
		CacheRead:         d.TotalCacheRead,
		CacheWrite:        d.TotalCacheWrite,
		SavingsUSD:        d.TotalCacheSavingsUSD,
		SavingsIncomplete: d.CacheSavingsIncomplete,
	}
	sig, err := s.Store.SessionSignalsByID(ctx, d.ID)
	if err != nil {
		return web.HeaderStats{}, err
	}
	return web.HeaderStats{Cache: cache, Signals: sig}, nil
}

func (s *Server) handleSessionPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		render(w, r, http.StatusNotFound, web.ErrorPage(s.pageFor(r, "Not found"), http.StatusNotFound, "Session not found."))
		return
	}
	d, msgs, tools, atts, subs, err := s.sessionView(r, id)
	if errors.Is(err, store.ErrNotFound) {
		render(w, r, http.StatusNotFound, web.ErrorPage(s.pageFor(r, "Not found"), http.StatusNotFound, "Session not found."))
		return
	}
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not load session."))
		return
	}
	// A bounded scalar (the GROUP BY runs in the database), so flagging a repeated
	// tool-call id costs one count query, not an in-process scan of every tool call.
	dupIDs, err := s.Store.DuplicateCallUIDCount(r.Context(), id)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not load session."))
		return
	}
	hs, err := s.sessionHeaderStats(r.Context(), d)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not load session."))
		return
	}
	title := fmt.Sprintf("Session #%d", d.ID)
	p, _ := principalFrom(r.Context())
	owner := p.UserID == d.OwnerID
	render(w, r, http.StatusOK, web.SessionPage(s.pageForNav(r, title, "sessions"), d, msgs, tools, atts, subs, hs, dupIDs, true, owner))
}

// handlePublishSession marks the owner's session public and redirects back to it.
func (s *Server) handlePublishSession(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	candidate, err := auth.NewPublicID()
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not publish session."))
		return
	}
	if _, err := s.Store.PublishSession(r.Context(), id, p.UserID, candidate); err != nil {
		// A session the caller does not own (or one that does not exist) is a 404,
		// not a hint that it belongs to someone else.
		render(w, r, http.StatusNotFound, web.ErrorPage(s.pageFor(r, "Not found"), http.StatusNotFound, "Session not found."))
		return
	}
	s.setNotice(w, "Published")
	http.Redirect(w, r, fmt.Sprintf("/sessions/%d", id), http.StatusSeeOther)
}

// handlePublishOverview marks the signed-in user's own usage overview public and
// redirects back to the account page, where the Publicity section then shows the
// /u/<username> link. The Open Graph preview card is not rendered here: it is
// rendered lazily the first time the card URL is fetched (a share unfurl) and cached
// from there, so publishing stays a single cheap write.
func (s *Server) handlePublishOverview(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := s.Store.PublishOverview(r.Context(), p.UserID); err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not publish overview."))
		return
	}
	s.setNotice(w, "Overview published")
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// handleUnpublishOverview hides the signed-in user's public overview. The URL is
// the username and never changes, so re-publishing later restores the same link.
func (s *Server) handleUnpublishOverview(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := s.Store.UnpublishOverview(r.Context(), p.UserID); err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not update overview."))
		return
	}
	s.setNotice(w, "Overview unpublished")
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// handlePublicOverview serves a user's published usage overview to logged-out
// viewers at /u/<username>. Every figure is scoped to that one account (UserIDs),
// so the page exposes neither another user's usage nor any session: it is the same
// aggregate panel the owner sees, with the per-user filter and session links
// absent. An unknown or unpublished username is a 404; a backend failure is a 500,
// not a "link expired", so a database hiccup is not misreported as a private page.
func (s *Server) handlePublicOverview(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	u, err := s.Store.PublicOverviewUser(r.Context(), username)
	if errors.Is(err, store.ErrNotFound) {
		render(w, r, http.StatusNotFound, web.PublicErrorPage(http.StatusNotFound, "This overview is not published, or the link has expired."))
		return
	}
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.PublicErrorPage(http.StatusInternalServerError, "Could not load overview."))
		return
	}
	rng := web.ParseRange(r.URL.Query().Get("range"))
	// The upper bound matches the card's (ogimage.DefaultUntil): the end of today, so
	// the page's headline totals cover exactly the days its heatmap draws (the grid
	// stops at today) rather than folding in a future-dated event no cell shows, and
	// the card advertised beside the default-range page reads the identical scope.
	now := time.Now()
	analytics, err := s.Store.Analytics(r.Context(), store.AnalyticsFilter{
		Since:   web.RangeSince(rng, now),
		Until:   ogimage.DefaultUntil(now),
		UserIDs: []int64{u.ID},
	})
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.PublicErrorPage(http.StatusInternalServerError, "Could not load overview."))
		return
	}
	og := web.OGMeta{
		Title:       u.Username + " · usage overview",
		Description: "A snapshot of " + u.Username + "'s AI coding-agent usage on akari.",
		URL:         s.baseURL(r) + web.PublicOverviewPath(u.Username),
	}
	// The preview card is a snapshot of the default trailing-year window, rendered on
	// demand and cached per user (not per range) for a short TTL, so it may trail the
	// live totals until the cache expires. It is advertised only on the default window
	// (a narrower ?range is a different view the year-window card does not represent);
	// the page still carries a well-formed summary card via its title and description
	// when the image is omitted.
	if rng == web.DefaultRange {
		og.Image = s.baseURL(r) + "/u/" + url.PathEscape(u.Username) + "/og.png"
	}
	render(w, r, http.StatusOK, web.PublicOverviewPage(u.Username, analytics, rng, og))
}

// handlePublicOverviewOGImage serves the Open Graph preview card for a published
// overview at /u/<username>/og.png. The card is rendered lazily and cached: a
// request served a card younger than the TTL returns the cached bytes; a miss or an
// expired card renders a fresh one on demand, stores it, and serves that. So a burst
// of crawler fetches after a share costs one render, not one per fetch, and a card
// nobody shares is never rendered at all. An unpublished or unknown account 404s,
// matching how /u/<username> itself resolves.
func (s *Server) handlePublicOverviewOGImage(w http.ResponseWriter, r *http.Request) {
	username := r.PathValue("username")
	now := time.Now()

	// One query resolves the user, checks the public gate, and reads any cached card
	// together. Folding the public check into the card read keeps the serve atomic: a
	// split (resolve the user, then read the card) would leave a window where a
	// concurrent unpublish slips between the two steps and a card is served for an
	// overview that just went private.
	u, cached, found, err := s.Store.PublicOverviewCard(r.Context(), username)
	if err != nil {
		http.Error(w, "Could not load preview image.", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	haveCache := cached.PNG != nil
	if haveCache && now.Sub(cached.GeneratedAt) < s.ogCacheTTL() {
		s.writeOGImage(w, cached.PNG)
		return
	}

	// Cache miss or expired: render on demand, store, and serve the fresh bytes.
	// A reparse rebuilding the projection makes a consistent snapshot impossible;
	// rather than cache a half-rebuilt total, Generate aborts. In that case serve the
	// last good card if we still hold one, else 404 (transient, clears once the
	// reparse finishes and a later request renders the card).
	//
	// Coalesce concurrent renders for this user through singleflight, so a burst of
	// unfurls on a cold or expired cache runs the render once and the rest serve its
	// result. renderOGImage detaches the shared render from any single request (so one
	// crawler dropping its connection cannot cancel it for the others) but bounds it
	// with a timeout, and lets this handler return early if its own request is
	// cancelled while the render continues for whoever is still waiting.
	png, genErr := s.renderOGImage(r.Context(), u, now)

	// The client may have disconnected mid-render: renderOGImage returns the request
	// context's error when it does. Nothing to serve and nothing broke, so return
	// quietly (and skip the gate re-read below, which that cancelled context would fail
	// anyway) rather than logging a spurious failure.
	if r.Context().Err() != nil {
		return
	}

	// Re-confirm the overview is still public before serving anything: an unpublish
	// during the render must 404, not unfurl a card (fresh or stale) for a now-private
	// overview. One gated read does double duty: it re-checks visibility and returns
	// the canonical cached card the stale-fallback branches serve. A real lookup error
	// is distinct from a closed gate: withhold the card either way, but surface the
	// backend failure rather than disguising it as a missing card.
	_, latest, stillPublic, gateErr := s.Store.PublicOverviewCard(r.Context(), username)
	switch {
	case gateErr != nil:
		log.Printf("overview og: public re-check for user %d (%s) failed: %v", u.ID, u.Username, gateErr)
		http.Error(w, "Could not load preview image.", http.StatusInternalServerError)
		return
	case !stillPublic:
		http.NotFound(w, r)
		return
	}

	switch {
	case genErr == nil:
		s.writeOGImage(w, png)
	case errors.Is(genErr, ogimage.ErrReparseInProgress):
		// A reparse blocked the fresh render. Serve the last good card if the gated
		// re-read still holds one, else 404 (transient, clears once the reparse ends).
		if latest.PNG != nil {
			s.writeOGImage(w, latest.PNG)
			return
		}
		http.NotFound(w, r)
	default:
		// A real render failure. Log it regardless of whether a stale card saves the
		// response: serving stale masks the failure from the crawler, but the refresh
		// still broke, and a persistently failing render must stay diagnosable rather
		// than hiding behind an ever-staler card. Then serve the last good card if we
		// hold one (it beats a 500 to a crawler), else report the error.
		log.Printf("overview og: render for user %d (%s) failed: %v", u.ID, u.Username, genErr)
		if latest.PNG != nil {
			s.writeOGImage(w, latest.PNG)
			return
		}
		http.Error(w, "Could not load preview image.", http.StatusInternalServerError)
	}
}

// landingOGCacheMaxAge is the Cache-Control window for the homepage card at
// /og.png. The card is static per binary (it reads no parsed data), so it only
// changes on deploy: a full day of crawler caching is safe, and mirrors the
// "changes only on deploy" lifetime the overview card gets through its TTL.
const landingOGCacheMaxAge = 86400

// handleLandingOGImage serves the Open Graph preview card for the instance root
// ("/") at /og.png. Unlike the per-user overview card, it carries no account data
// (just the wordmark, the product headline, and a decorative band), so it is
// static per binary: ogimage.Landing renders it once and memoizes the bytes, and
// there is nothing to gate on a reparse or scope to a user. A render failure is a
// missing font asset in the binary, an internal error, not a 404.
func (s *Server) handleLandingOGImage(w http.ResponseWriter, r *http.Request) {
	png, err := ogimage.Landing()
	if err != nil {
		log.Printf("landing og: render failed: %v", err)
		http.Error(w, "Could not load preview image.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", landingOGCacheMaxAge))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

// ogRenderTimeout bounds a single on-demand card render (its analytics snapshot, the
// raster, and the cache write). The render is detached from the request that triggers
// it so a dropped crawler connection cannot cancel it for the other waiters, so it
// needs its own deadline: without one a stuck query would pin the singleflight leader
// and every same-user waiter, and could stall shutdown. Rendering is normally
// sub-second, so 30s is a generous safety ceiling well above the expected duration.
const ogRenderTimeout = 30 * time.Second

// renderOGImage renders and caches one user's preview card through the per-user
// singleflight group, so concurrent misses for the same overview share a single
// render rather than each running the full year-window analytics and raster. The
// waiters receive the same bytes and error the leader produced; ogimage.Generate
// already reconciles a losing guarded write to the canonical cached card, so every
// caller here serves what the cache holds.
//
// The shared render runs under a bounded context detached from any single caller
// (context.WithoutCancel plus a timeout), so one request disconnecting does not cancel
// it for the others, yet it cannot run unbounded. Each caller still waits on its own
// request context: a crawler that drops its connection returns promptly with that
// context's error while the detached render continues for whoever remains.
func (s *Server) renderOGImage(ctx context.Context, u store.User, now time.Time) ([]byte, error) {
	ch := s.ogRender.DoChan(strconv.FormatInt(u.ID, 10), func() (any, error) {
		renderCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ogRenderTimeout)
		defer cancel()
		return ogimage.Generate(renderCtx, s.Store, u, now)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.([]byte), nil
	}
}

// ogCacheTTL is how long a rendered preview card is served before a request
// re-renders it. It honors the configured value and falls back to a sane default, so
// a zero-value config (as the tests construct) still caches rather than rendering on
// every request.
func (s *Server) ogCacheTTL() time.Duration {
	if s.Cfg.OGCacheTTL > 0 {
		return s.Cfg.OGCacheTTL
	}
	return time.Hour
}

// writeOGImage serves the card bytes as a PNG. The Cache-Control window mirrors the
// server-side TTL, so a crawler's repeat unfurls stay off the render path for about
// as long as the cached card is considered fresh, without pinning a stale card
// longer.
func (s *Server) writeOGImage(w http.ResponseWriter, png []byte) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Length", strconv.Itoa(len(png)))
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(s.ogCacheTTL().Seconds())))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(png)
}

// handleUnpublishSession returns the owner's session to internal visibility.
func (s *Server) handleUnpublishSession(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.Store.UnpublishSession(r.Context(), id, p.UserID); err != nil {
		render(w, r, http.StatusNotFound, web.ErrorPage(s.pageFor(r, "Not found"), http.StatusNotFound, "Session not found."))
		return
	}
	s.setNotice(w, "Unpublished")
	http.Redirect(w, r, fmt.Sprintf("/sessions/%d", id), http.StatusSeeOther)
}

// handleDeleteSession removes a session. The owner may delete their own session;
// an admin may delete any. Its CAS blobs are reclaimed by a later sweep.
func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d, err := s.Store.SessionDetailByID(r.Context(), id)
	if err != nil {
		render(w, r, http.StatusNotFound, web.ErrorPage(s.pageFor(r, "Not found"), http.StatusNotFound, "Session not found."))
		return
	}
	if p.UserID != d.OwnerID {
		// Only the owner or an admin may delete a session.
		if u, err := s.Store.UserByID(r.Context(), p.UserID); err != nil || !u.IsAdmin {
			render(w, r, http.StatusForbidden, web.ErrorPage(s.pageFor(r, "Forbidden"), http.StatusForbidden, "You cannot delete this session."))
			return
		}
	}
	if err := s.Store.DeleteSession(r.Context(), id); err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not delete session."))
		return
	}
	s.setNotice(w, "Session deleted")
	http.Redirect(w, r, fmt.Sprintf("/projects/%d", d.ProjectID), http.StatusSeeOther)
}

// handlePublicSession serves a published session to logged-out viewers, reached
// only through the unguessable public id. It never exposes the numeric id and
// shows only subagents that are themselves public.
func (s *Server) handlePublicSession(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("public_id")
	d, err := s.Store.SessionDetailByPublicID(r.Context(), pid)
	if err != nil {
		render(w, r, http.StatusNotFound, web.PublicErrorPage(http.StatusNotFound, "This session is not published, or the link has expired."))
		return
	}
	msgs, err := s.Store.Messages(r.Context(), d.ID)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.PublicErrorPage(http.StatusInternalServerError, "Could not load session."))
		return
	}
	tools, err := s.Store.ToolCalls(r.Context(), d.ID)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.PublicErrorPage(http.StatusInternalServerError, "Could not load session."))
		return
	}
	atts, err := s.Store.Attachments(r.Context(), d.ID)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.PublicErrorPage(http.StatusInternalServerError, "Could not load session."))
		return
	}
	subs, err := s.Store.Subagents(r.Context(), d.ID)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.PublicErrorPage(http.StatusInternalServerError, "Could not load session."))
		return
	}
	// Only public subagents may appear on a public page; a public parent does not
	// make its children public.
	var publicSubs []store.SessionSummary
	for _, sub := range subs {
		if sub.Visibility == "public" && sub.PublicID != nil {
			publicSubs = append(publicSubs, sub)
		}
	}
	hs, err := s.sessionHeaderStats(r.Context(), d)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.PublicErrorPage(http.StatusInternalServerError, "Could not load session."))
		return
	}
	render(w, r, http.StatusOK, web.PublicSessionPage(d, msgs, web.ToolsByOrdinal(tools), web.AttachmentsByOrdinal(atts), publicSubs, hs))
}

// handleSessionBody serves just the live-updating body fragment, re-fetched by
// the page over SSE when new bytes are parsed.
func (s *Server) handleSessionBody(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d, msgs, tools, atts, subs, err := s.sessionView(r, id)
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
	// The live fragment re-renders the stat header on every SSE update. The Cache tile now
	// comes off the same SessionDetail the body already loaded, so it tracks the session's
	// growing usage in step with the Tokens tile beside it without a second read; only the
	// stored quality signals are fetched here.
	hs, err := s.sessionHeaderStats(r.Context(), d)
	if err != nil {
		http.Error(w, "Could not load session.", http.StatusInternalServerError)
		return
	}
	render(w, r, http.StatusOK, web.SessionMain(d, msgs, tools, atts, subs, hs))
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

func (s *Server) handleAccountPage(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	tokens, err := s.Store.ListAPITokens(r.Context(), p.UserID)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not load tokens."))
		return
	}
	grants, err := s.Store.ListOAuthGrants(r.Context(), p.UserID)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not load connected apps."))
		return
	}
	page := s.pageForNav(r, "Account", "account")
	// Invites are admin-only machinery: skip the query entirely for a non-admin
	// viewer rather than loading a list the page never renders.
	var invites []store.Invite
	if page.IsAdmin {
		invites, err = s.Store.ListInvites(r.Context())
		if err != nil {
			render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not load invites."))
			return
		}
	}
	// Freshly minted secrets are passed once via short-lived flash cookies, then
	// cleared, so a page reload does not keep showing them.
	newToken := readFlash(w, r, "akari_new_token")
	newInvite := readFlash(w, r, "akari_new_invite")
	st := s.reparser.Status()
	rp := web.ReparseView{InProgress: st.InProgress, Done: st.Done, Total: st.Total, Failed: st.Failed}
	render(w, r, http.StatusOK, web.AccountPage(page, tokens, grants, invites, newToken, newInvite, rp))
}

// Login and register, form (HTML) variants. These mirror the JSON handlers but
// set the session cookie and redirect instead of returning JSON.

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if p, ok := s.resolve(r); ok && p.Scope == scopeFull {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	next := safeNext(r.URL.Query().Get("next"))
	render(w, r, http.StatusOK, web.LoginPage(web.Page{Title: "Log in"}, next, ""))
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		render(w, r, http.StatusBadRequest, web.LoginPage(web.Page{Title: "Log in"}, "/", "Invalid form."))
		return
	}
	next := safeNext(r.PostFormValue("next"))
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	u, err := s.Store.UserByUsername(r.Context(), username)
	if err != nil {
		render(w, r, http.StatusUnauthorized, web.LoginPage(web.Page{Title: "Log in"}, next, "Invalid credentials."))
		return
	}
	if !u.HasPassword() {
		// A federated account (proxy-provisioned) has no local password and cannot
		// use this form; it signs in through its external source. Refuse without
		// revealing the account exists.
		render(w, r, http.StatusUnauthorized, web.LoginPage(web.Page{Title: "Log in"}, next, "Invalid credentials."))
		return
	}
	ok, err := auth.VerifyPassword(password, u.PasswordHash)
	if err != nil || !ok {
		render(w, r, http.StatusUnauthorized, web.LoginPage(web.Page{Title: "Log in"}, next, "Invalid credentials."))
		return
	}
	if err := s.startSession(w, r, u.ID); err != nil {
		render(w, r, http.StatusInternalServerError, web.LoginPage(web.Page{Title: "Log in"}, next, "Could not start session."))
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleRegisterPage(w http.ResponseWriter, r *http.Request) {
	render(w, r, http.StatusOK, web.RegisterPage(web.Page{Title: "Register"}, ""))
}

func (s *Server) handleRegisterForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		render(w, r, http.StatusBadRequest, web.RegisterPage(web.Page{Title: "Register"}, "Invalid form."))
		return
	}
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	invite := strings.TrimSpace(r.PostFormValue("invite_token"))
	if username == "" || password == "" {
		render(w, r, http.StatusBadRequest, web.RegisterPage(web.Page{Title: "Register"}, "Username and password are required."))
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.RegisterPage(web.Page{Title: "Register"}, "Could not create account."))
		return
	}
	inviteHash := ""
	if invite != "" {
		inviteHash = auth.HashToken(invite)
	}
	u, err := s.Store.Register(r.Context(), username, hash, inviteHash)
	switch {
	case errors.Is(err, store.ErrInvalidInvite):
		render(w, r, http.StatusForbidden, web.RegisterPage(web.Page{Title: "Register"}, "A valid invite token is required."))
		return
	case isUniqueViolation(err):
		render(w, r, http.StatusConflict, web.RegisterPage(web.Page{Title: "Register"}, "That username is taken."))
		return
	case err != nil:
		render(w, r, http.StatusInternalServerError, web.RegisterPage(web.Page{Title: "Register"}, "Could not create account."))
		return
	}
	if err := s.startSession(w, r, u.ID); err != nil {
		render(w, r, http.StatusInternalServerError, web.RegisterPage(web.Page{Title: "Register"}, "Could not start session."))
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogoutForm(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		_ = s.Store.DeleteWebSession(r.Context(), auth.HashToken(c.Value))
	}
	s.clearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// Account form actions: create/revoke tokens and create invites, then redirect
// back to the account page (passing freshly minted secrets via flash cookies).

func (s *Server) handleCreateTokenForm(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	scope := r.PostFormValue("scope")
	if scope != scopeIngest && scope != scopeFull && scope != scopeRead {
		scope = scopeIngest
	}
	if name == "" {
		name = "token"
	}
	token, err := auth.NewToken()
	if err != nil {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	if _, err := s.Store.CreateAPIToken(r.Context(), p.UserID, name, scope, auth.HashToken(token)); err != nil {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	s.setFlash(w, "akari_new_token", token)
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

func (s *Server) handleRevokeTokenForm(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	// Surface a revocation failure instead of redirecting as if it worked: a silent
	// redirect would tell the user the token is gone while it stays live, matching the
	// connection- and invite-revoke handlers.
	if err := s.Store.RevokeAPIToken(r.Context(), p.UserID, id); err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not revoke the token. Try again."))
		return
	}
	s.setNotice(w, "Token revoked")
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// handleRevokeConnectionForm disconnects an OAuth client from the account, revoking
// every token the grant holds. It is scoped to the signed-in user, so it can only
// disconnect the user's own connections.
func (s *Server) handleRevokeConnectionForm(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	clientID := r.PathValue("client_id")
	if clientID != "" {
		// Surface a revocation failure instead of redirecting as if it worked: a
		// silent redirect would tell the user the app is disconnected while its
		// tokens stay live.
		if err := s.Store.RevokeOAuthGrant(r.Context(), p.UserID, clientID); err != nil {
			render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not disconnect the app. Try again."))
			return
		}
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

func (s *Server) handleCreateInviteForm(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	token, err := auth.NewToken()
	if err != nil {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	if _, err := s.Store.CreateInvite(r.Context(), auth.HashToken(token), p.UserID, "", nil); err != nil {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	s.setFlash(w, "akari_new_invite", token)
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// handleRevokeInviteForm deletes an invite token by id. Deletion (not a revoked
// flag, unlike API tokens) is correct here: an invite carries no history worth
// keeping once it will never be redeemed, and ListInvites has nothing left to
// join against it for.
func (s *Server) handleRevokeInviteForm(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	// Surface a deletion failure instead of redirecting as if it worked: a silent
	// redirect would tell the admin the invite is gone while it stays redeemable,
	// matching the connection-revoke handler's ErrorPage on failure.
	if err := s.Store.RevokeInvite(r.Context(), id); err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not revoke the invite. Try again."))
		return
	}
	s.setNotice(w, "Invite revoked")
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

// safeNext bounds a post-login redirect target to a local path, so a crafted
// next= cannot bounce the user to another origin.
func safeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
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
