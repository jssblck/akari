package httpapi

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jssblck/akari/internal/server/auth"
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
		s.renderError(w, r, http.StatusInternalServerError, "Could not publish session.")
		return
	}
	if _, err := s.Store.PublishSession(r.Context(), id, p.UserID, candidate); err != nil {
		// A session the caller does not own (or one that does not exist) is a 404,
		// not a hint that it belongs to someone else.
		s.renderError(w, r, http.StatusNotFound, "Session not found.")
		return
	}
	s.setNotice(w, r, "Published")
	http.Redirect(w, r, s.href(r, fmt.Sprintf("/sessions/%d", id)), http.StatusSeeOther)
}

// handlePublishOverview marks the signed-in user's own usage overview public and
// redirects back to the account page, where the Publicity section then shows the
// /u/<username> link. The Open Graph preview card is not rendered here: it is
// rendered lazily the first time the card URL is fetched (a share unfurl) and cached
// from there, so publishing stays a single cheap write.
func (s *Server) handlePublishOverview(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := s.Store.PublishOverview(r.Context(), p.UserID); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Could not publish overview.")
		return
	}
	s.analyticsSnapshots.invalidate(analyticsScope{kind: analyticsUserScope, id: p.UserID})
	s.setNotice(w, r, "Overview published")
	http.Redirect(w, r, s.href(r, "/account"), http.StatusSeeOther)
}

// handleUnpublishOverview hides the signed-in user's public overview. The URL is
// the username and never changes, so re-publishing later restores the same link.
func (s *Server) handleUnpublishOverview(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if err := s.Store.UnpublishOverview(r.Context(), p.UserID); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Could not update overview.")
		return
	}
	s.analyticsSnapshots.invalidate(analyticsScope{kind: analyticsUserScope, id: p.UserID})
	s.setNotice(w, r, "Overview unpublished")
	http.Redirect(w, r, s.href(r, "/account"), http.StatusSeeOther)
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
			s.renderError(w, r, http.StatusNotFound, "Project not found.")
			return
		}
		s.renderError(w, r, http.StatusInternalServerError, "Could not publish overview.")
		return
	}
	s.analyticsSnapshots.invalidate(analyticsScope{kind: analyticsProjectScope, id: id})
	s.setNotice(w, r, "Overview published")
	http.Redirect(w, r, s.href(r, fmt.Sprintf("/projects/%d", id)), http.StatusSeeOther)
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
			s.renderError(w, r, http.StatusNotFound, "Project not found.")
			return
		}
		s.renderError(w, r, http.StatusInternalServerError, "Could not update overview.")
		return
	}
	s.analyticsSnapshots.invalidate(analyticsScope{kind: analyticsProjectScope, id: id})
	s.setNotice(w, r, "Overview unpublished")
	http.Redirect(w, r, s.href(r, fmt.Sprintf("/projects/%d", id)), http.StatusSeeOther)
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
		renderPublicError(w, r, http.StatusNotFound, "This project overview is not published, or the link has expired.")
		return
	}
	// Resolve the publication gate before consulting the shared snapshot. The cached
	// generation contains aggregate data only, but it can never authorize its own use:
	// every request must still prove that the project is public.
	proj, err := s.Store.PublicProjectOverview(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		renderPublicError(w, r, http.StatusNotFound, "This project overview is not published, or the link has expired.")
		return
	}
	if err != nil {
		renderPublicError(w, r, http.StatusInternalServerError, "Could not load project overview.")
		return
	}
	rng := web.ParseRange(r.URL.Query().Get("range"))
	started := time.Now()
	snapshot, meta, err := s.analyticsSnapshots.get(r.Context(), analyticsSnapshotKey{
		scope: analyticsScope{kind: analyticsProjectScope, id: id}, rangeKey: rng,
	})
	if err != nil {
		status, respond := analyticsSnapshotErrorStatus(w, r, err)
		if respond {
			renderPublicError(w, r, status, "Could not load project overview.")
		}
		return
	}
	observeAnalyticsSnapshot(w, started, meta, s.analyticsSnapshots.freshFor, s.analyticsSnapshots.staleFor)
	analytics, insights := snapshot.analytics, snapshot.insights
	og := web.OGMeta{
		Title:       web.ProjectTitle(proj) + " · usage overview",
		Description: "A snapshot of AI coding-agent usage on " + web.ProjectTitle(proj) + " on akari.",
		URL:         s.absURL(r, web.PublicProjectPath(proj.ID)),
	}
	// The preview card is a snapshot of the default trailing-year window, rendered on
	// demand and cached per project (not per range) for a short TTL, so it may trail the
	// live totals until the cache expires. It is advertised only on the default window (a
	// narrower ?range is a different view the year-window card does not represent), exactly
	// as the public user overview advertises its card; the page still carries a well-formed
	// summary card via its title and description when the image is omitted.
	if rng == web.DefaultRange {
		og.Image = s.absURL(r, web.PublicProjectOGPath(proj.ID))
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
		renderPublicError(w, r, http.StatusNotFound, "This overview is not published, or the link has expired.")
		return
	}
	if err != nil {
		renderPublicError(w, r, http.StatusInternalServerError, "Could not load overview.")
		return
	}
	rng := web.ParseRange(r.URL.Query().Get("range"))
	started := time.Now()
	snapshot, meta, err := s.analyticsSnapshots.get(r.Context(), analyticsSnapshotKey{
		scope: analyticsScope{kind: analyticsUserScope, id: u.ID}, rangeKey: rng,
	})
	if err != nil {
		status, respond := analyticsSnapshotErrorStatus(w, r, err)
		if respond {
			renderPublicError(w, r, status, "Could not load overview.")
		}
		return
	}
	observeAnalyticsSnapshot(w, started, meta, s.analyticsSnapshots.freshFor, s.analyticsSnapshots.staleFor)
	analytics := snapshot.analytics
	og := web.OGMeta{
		Title:       u.Username + " · usage overview",
		Description: "A snapshot of " + u.Username + "'s AI coding-agent usage on akari.",
		URL:         s.absURL(r, web.PublicOverviewPath(u.Username)),
	}
	// The preview card is a snapshot of the default trailing-year window, rendered on
	// demand and cached per user (not per range) for a short TTL, so it may trail the
	// live totals until the cache expires. It is advertised only on the default window
	// (a narrower ?range is a different view the year-window card does not represent);
	// the page still carries a well-formed summary card via its title and description
	// when the image is omitted.
	if rng == web.DefaultRange {
		og.Image = s.absURL(r, web.PublicOverviewOGPath(u.Username))
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
		s.renderError(w, r, http.StatusNotFound, "Session not found.")
		return
	}
	s.setNotice(w, r, "Unpublished")
	http.Redirect(w, r, s.href(r, fmt.Sprintf("/sessions/%d", id)), http.StatusSeeOther)
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
		s.renderError(w, r, http.StatusNotFound, "Session not found.")
		return
	}
	if p.UserID != d.OwnerID {
		// Only the owner or an admin may delete a session.
		if u, err := s.Store.UserByID(r.Context(), p.UserID); err != nil || !u.IsAdmin {
			s.renderError(w, r, http.StatusForbidden, "You cannot delete this session.")
			return
		}
	}
	if err := s.Store.DeleteSession(r.Context(), id); err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Could not delete session.")
		return
	}
	s.setNotice(w, r, "Session deleted")
	http.Redirect(w, r, s.href(r, fmt.Sprintf("/projects/%d", d.ProjectID)), http.StatusSeeOther)
}

// handlePublicSession serves a bounded tail of a published session to logged-out
// viewers. The header and transcript window come from one projection snapshot.
func (s *Server) handlePublicSession(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("public_id")
	snap, err := s.Store.PublicSessionByID(r.Context(), pid, nil)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			renderPublicError(w, r, http.StatusNotFound, "This session is not published, or the link has expired.")
			return
		}
		renderPublicError(w, r, http.StatusInternalServerError, "Could not load session.")
		return
	}
	v := publicSessionViewFrom(snap)
	d := v.Detail
	// A published session's public id is non-nil (visibility gates on it), so the card
	// URL and canonical URL both resolve. Unlike the overview and project cards the
	// session card has no range, so it is advertised unconditionally: there is one card
	// per session, not one per window. The title and description mirror the page's own
	// head, so the link unfurls with the same identity whether or not a crawler fetches
	// the image.
	og := web.OGMeta{
		Title:       web.SessionPageTitle(d),
		Description: "A shared " + d.Agent + " session on " + web.SessionProjectLabel(d) + " in akari.",
		URL:         s.absURL(r, web.PublicPath(*d.PublicID)),
		Image:       s.absURL(r, web.PublicSessionOGPath(*d.PublicID)),
	}
	render(w, r, http.StatusOK, web.PublicSessionPage(v, og))
}

// handlePublicSessionBody pages backward through a public transcript. The browser
// carries the projection revision from the page it already holds. A mismatch means
// a rebuild replaced the ordinal space, so the response replaces the complete
// bounded body instead of appending rows from a different projection.
func (s *Server) handlePublicSessionBody(w http.ResponseWriter, r *http.Request) {
	before, err := strconv.Atoi(r.URL.Query().Get("before"))
	if err != nil || before < 0 {
		http.Error(w, "bad before cursor", http.StatusBadRequest)
		return
	}
	revision, err := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if err != nil || revision < 0 {
		http.Error(w, "bad projection revision", http.StatusBadRequest)
		return
	}
	snap, err := s.Store.PublicSessionByID(r.Context(), r.PathValue("public_id"), &before)
	if err != nil {
		fragmentPublicError(w, err)
		return
	}
	if revision != snap.ProjectionRevision {
		fresh, err := s.Store.PublicSessionByID(r.Context(), r.PathValue("public_id"), nil)
		if err != nil {
			fragmentPublicError(w, err)
			return
		}
		w.Header().Set("HX-Retarget", "#session-body")
		w.Header().Set("HX-Reswap", "innerHTML")
		w.Header().Set("X-Akari-Projection-Changed", "1")
		render(w, r, http.StatusOK, web.PublicSessionBody(publicSessionViewFrom(fresh)))
		return
	}
	render(w, r, http.StatusOK, web.PublicTranscriptEarlier(publicSessionViewFrom(snap)))
}

func publicSessionViewFrom(snap store.PublicSessionSnapshot) web.SessionView {
	v := web.SessionView{
		Detail:             snap.Audit.Detail,
		Header:             sessionHeaderStats(snap.Audit.Detail, snap.Audit.Signals, snap.Audit.Fallbacks),
		Outline:            snap.Outline,
		Tools:              web.ToolsByOrdinal(snap.Tools),
		ProjectionRevision: snap.ProjectionRevision,
	}
	// Publishing a parent does not publish its children. Filter rows read in the
	// same snapshot before handing the view to the public renderer.
	for _, sub := range snap.Audit.Subagents {
		if sub.Visibility == "public" && sub.PublicID != nil {
			v.Subagents = append(v.Subagents, sub)
		}
	}
	v.SetPage(snap.Page)
	return v
}

func fragmentPublicError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "Session not found.", http.StatusNotFound)
		return
	}
	http.Error(w, "Could not load session.", http.StatusInternalServerError)
}
