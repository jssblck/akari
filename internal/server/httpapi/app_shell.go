package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jssblck/akari/internal/server/frontend"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/web"
)

func (s *Server) handleGuideRoute(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.PathValue("slug"), ".md") {
		s.handleGuidePage(w, r)
		return
	}
	s.handleAppShell(w, r)
}

// handleAppShell serves the same embedded React entry document for every client-side
// route. The homepage deliberately remains outside this path and continues to render
// from landing.templ.
func (s *Server) handleAppShell(w http.ResponseWriter, _ *http.Request) {
	index, err := frontend.Index()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load embedded frontend")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-cache")
	}
	_, _ = w.Write(index)
}

func (s *Server) handlePublicUserShell(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	user, err := s.Store.PublicOverviewUser(r.Context(), r.PathValue("username"))
	if err != nil {
		handlePublicShellLookupError(w, err)
		return
	}
	title := user.Username + " usage overview"
	s.handleAppShellMetadata(w, frontend.Metadata{
		Title: title, Description: "A snapshot of " + user.Username + "'s AI coding-agent usage on akari.",
		URL:   s.baseURL(r) + web.PublicOverviewPath(user.Username),
		Image: s.baseURL(r) + web.PublicOverviewOGPath(user.Username),
	})
}

func (s *Server) handlePublicProjectShell(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	project, err := s.Store.PublicProjectOverview(r.Context(), id)
	if err != nil {
		handlePublicShellLookupError(w, err)
		return
	}
	name := web.ProjectTitle(project)
	s.handleAppShellMetadata(w, frontend.Metadata{
		Title: name + " usage overview", Description: "A snapshot of AI coding-agent usage on " + name + " on akari.",
		URL: s.baseURL(r) + web.PublicProjectPath(project.ID), Image: s.baseURL(r) + web.PublicProjectOGPath(project.ID),
	})
}

func (s *Server) handlePublicSessionShell(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	detail, err := s.Store.SessionDetailByPublicID(r.Context(), r.PathValue("public_id"))
	if err != nil {
		handlePublicShellLookupError(w, err)
		return
	}
	publicID := r.PathValue("public_id")
	s.handleAppShellMetadata(w, frontend.Metadata{
		Title: web.SessionPageTitle(detail), Description: "A shared " + detail.Agent + " session on " + web.SessionProjectLabel(detail) + " in akari.",
		URL: s.baseURL(r) + web.PublicPath(publicID), Image: s.baseURL(r) + web.PublicSessionOGPath(publicID),
	})
}

func (s *Server) handleAppShellMetadata(w http.ResponseWriter, meta frontend.Metadata) {
	index, err := frontend.Document(meta)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "load embedded frontend")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(index)
}

func handlePublicShellLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "Not found", http.StatusNotFound)
		return
	}
	http.Error(w, "Could not load published view", http.StatusInternalServerError)
}

// requireAppShell redirects browser navigation to login while API requests keep the
// JSON 401 behavior supplied by requireFull. This makes copied private URLs work on a
// cold load without coupling client routing to authentication middleware.
func (s *Server) requireAppShell(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.resolve(r)
		if !ok || p.Scope != scopeFull {
			http.Redirect(w, r, "/login?next="+r.URL.RequestURI(), http.StatusSeeOther)
			return
		}
		setPrivateNoStore(w)
		next(w, s.withPrincipal(r, p))
	}
}
