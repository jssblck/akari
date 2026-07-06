package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/ogimage"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/web"
)

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

// handlePublishProjectOverview marks a project's usage overview public and redirects
// back to the project page. Projects are fleet-global rather than owned, so any
// full-scope caller may publish (the route's requireFull guard); unlike a session
// publish there is no owner check. A missing project is a 404.
func (s *Server) handlePublishProjectOverview(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.Store.PublishProjectOverview(r.Context(), id); err != nil {
		// A missing project is a 404; a backend failure is a 500, so a database error
		// is not misreported as "project not found" (which would read as the caller's
		// mistake rather than the server's).
		if errors.Is(err, store.ErrNotFound) {
			render(w, r, http.StatusNotFound, web.ErrorPage(s.pageFor(r, "Not found"), http.StatusNotFound, "Project not found."))
			return
		}
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not publish overview."))
		return
	}
	s.setNotice(w, "Overview published")
	http.Redirect(w, r, fmt.Sprintf("/projects/%d", id), http.StatusSeeOther)
}

// handleUnpublishProjectOverview hides a project's public overview. The URL is the
// project id and never changes, so re-publishing later restores the same /p/<id>.
func (s *Server) handleUnpublishProjectOverview(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.Store.UnpublishProjectOverview(r.Context(), id); err != nil {
		// Split ErrNotFound (a 404) from a backend failure (a 500), so a database error
		// while making a project private is not disguised as a missing project.
		if errors.Is(err, store.ErrNotFound) {
			render(w, r, http.StatusNotFound, web.ErrorPage(s.pageFor(r, "Not found"), http.StatusNotFound, "Project not found."))
			return
		}
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not update overview."))
		return
	}
	s.setNotice(w, "Overview unpublished")
	http.Redirect(w, r, fmt.Sprintf("/projects/%d", id), http.StatusSeeOther)
}

// handlePublicProject serves a project's published usage overview to logged-out
// viewers at /p/<id>. Every figure is scoped to that one project (ProjectID) across
// every account, so the page exposes the repo's aggregate usage and quality shape but
// neither a session nor which accounts ran in it: the session list and the by-user
// breakdown are dropped (see PublicProjectPage). An unknown or unpublished id is a
// 404; a backend failure is a 500, not a "link expired", so a database hiccup is not
// misreported as a private page.
func (s *Server) handlePublicProject(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		render(w, r, http.StatusNotFound, web.PublicErrorPage(http.StatusNotFound, "This project overview is not published, or the link has expired."))
		return
	}
	// The public gate and the aggregate reads below are deliberately not one atomic
	// transaction. This matches handlePublicOverview: a concurrent unpublish landing
	// between the gate read and the aggregate reads at worst serves one more
	// already-in-flight aggregate page (no session, no secret, just usage totals the
	// owner made public moments ago) before the next request 404s. The atomic
	// gate-and-read fold is reserved for the cached OG card (PublicOverviewCard), where
	// a stale read would persist in a TTL cache rather than vanishing on the next load.
	proj, err := s.Store.PublicProjectOverview(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		render(w, r, http.StatusNotFound, web.PublicErrorPage(http.StatusNotFound, "This project overview is not published, or the link has expired."))
		return
	}
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.PublicErrorPage(http.StatusInternalServerError, "Could not load project overview."))
		return
	}
	// The window bounds both the usage panel and the quality band, exactly as the
	// signed-in project page pairs them; the public page carries no filter toolbar, so
	// the scope is the whole project (no agent/user/machine narrowing).
	rng := web.ParseRange(r.URL.Query().Get("range"))
	now := time.Now()
	since := web.RangeSince(rng, now)
	// One filter scopes both projections the public page renders (the usage panel and the
	// quality band), so they cannot drift on the same page: same project, same window, same
	// end-of-today upper bound, same OmitUsers. The upper bound (matching the public user
	// overview) makes the usage headline cover exactly the days the trailing-year heatmap
	// draws (the grid stops at today), and applying it to the quality band too keeps the two
	// on one window rather than letting a future-started session count in the band while its
	// future usage is excluded from the panel. OmitUsers skips the per-user aggregates the
	// public page never renders (the by-user cost split and the People leaderboard), so the
	// reads do not build per-user results proportional to the project's user count only to
	// discard them.
	//
	// This shared upper bound is the one intentional gap from the signed-in project page,
	// which leaves both projections unbounded above so its usage panel reconciles with its
	// live session table (windowed on the same unbounded dated-usage base). The two surfaces
	// therefore agree for all real, past-dated data and can differ only by a future-dated
	// event, a malformed-transcript case that does not occur in practice (occurred_at and
	// started_at are stamped from the transcript's own turns). See
	// TestPublicAndAuthedProjectAnalyticsReconcile, which pins that gap.
	af := store.AnalyticsFilter{ProjectID: id, Since: since, Until: ogimage.DefaultUntil(now), OmitUsers: true}
	analytics, err := s.Store.Analytics(r.Context(), af)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.PublicErrorPage(http.StatusInternalServerError, "Could not load project overview."))
		return
	}
	insights, err := s.Store.Insights(r.Context(), af)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.PublicErrorPage(http.StatusInternalServerError, "Could not load project overview."))
		return
	}
	og := web.OGMeta{
		Title:       web.ProjectTitle(proj) + " · usage overview",
		Description: "A snapshot of AI coding-agent usage on " + web.ProjectTitle(proj) + " on akari.",
		URL:         s.baseURL(r) + web.PublicProjectPath(proj.ID),
	}
	// The preview card is a snapshot of the default trailing-year window, rendered on
	// demand and cached per project (not per range) for a short TTL, so it may trail the
	// live totals until the cache expires. It is advertised only on the default window (a
	// narrower ?range is a different view the year-window card does not represent), exactly
	// as the public user overview advertises its card; the page still carries a well-formed
	// summary card via its title and description when the image is omitted.
	if rng == web.DefaultRange {
		og.Image = s.baseURL(r) + web.PublicProjectOGPath(proj.ID)
	}
	render(w, r, http.StatusOK, web.PublicProjectPage(proj, analytics, insights, rng, og))
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
		og.Image = s.baseURL(r) + web.PublicOverviewOGPath(u.Username)
	}
	render(w, r, http.StatusOK, web.PublicOverviewPage(u.Username, analytics, rng, og))
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
	var publicSubs []store.SubagentRow
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
	// A published session's public id is non-nil (visibility gates on it), so the card
	// URL and canonical URL both resolve. Unlike the overview and project cards the
	// session card has no range, so it is advertised unconditionally: there is one card
	// per session, not one per window. The title and description mirror the page's own
	// head, so the link unfurls with the same identity whether or not a crawler fetches
	// the image.
	og := web.OGMeta{
		Title:       web.SessionPageTitle(d),
		Description: "A shared " + d.Agent + " session on " + web.SessionProjectLabel(d) + " in akari.",
		URL:         s.baseURL(r) + web.PublicPath(*d.PublicID),
		Image:       s.baseURL(r) + web.PublicSessionOGPath(*d.PublicID),
	}
	render(w, r, http.StatusOK, web.PublicSessionPage(d, msgs, web.ToolsByOrdinal(tools), web.AttachmentsByOrdinal(atts), publicSubs, hs, og))
}
