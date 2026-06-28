package httpapi

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/jssblck/akari/internal/server/auth"
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
	}
	return pg
}

// render writes a templ component, mapping a render error to a 500.
func render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := c.Render(r.Context(), w); err != nil {
		// The header is already written; nothing left but to log via the default
		// http error path is impossible, so swallow (the connection will close).
		_ = err
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		render(w, r, http.StatusNotFound, web.ErrorPage(s.pageFor(r, "Not found"), http.StatusNotFound, "Page not found."))
		return
	}
	projects, err := s.Store.ListProjects(r.Context())
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not load projects."))
		return
	}
	render(w, r, http.StatusOK, web.ProjectsPage(s.pageFor(r, "Projects"), projects))
}

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

	filter := store.SessionFilter{
		ProjectID: id,
		Agent:     strings.TrimSpace(r.URL.Query().Get("agent")),
		Machine:   strings.TrimSpace(r.URL.Query().Get("machine")),
		Username:  strings.TrimSpace(r.URL.Query().Get("user")),
	}
	sessions, err := s.Store.ListSessions(r.Context(), filter)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not load sessions."))
		return
	}

	// An htmx request swaps only the session list; a normal load renders the page.
	if r.Header.Get("HX-Request") == "true" {
		render(w, r, http.StatusOK, web.SessionList(sessions))
		return
	}

	facets, err := s.Store.SessionFacets(r.Context(), id)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not load filters."))
		return
	}
	wf := web.Facets{Agents: facets.Agents, Machines: facets.Machines, Users: facets.Users}
	render(w, r, http.StatusOK, web.ProjectPage(s.pageFor(r, proj.RemoteKey), proj, sessions, wf, filter))
}

// sessionView loads everything the session page (and its live body fragment)
// needs: detail, transcript, tool metadata grouped by message, and subagents.
func (s *Server) sessionView(r *http.Request, id int64) (store.SessionDetail, []store.Message, map[int][]store.ToolCallView, []store.SessionSummary, error) {
	d, err := s.Store.SessionDetailByID(r.Context(), id)
	if err != nil {
		return d, nil, nil, nil, err
	}
	msgs, err := s.Store.Messages(r.Context(), id)
	if err != nil {
		return d, nil, nil, nil, err
	}
	tools, err := s.Store.ToolCalls(r.Context(), id)
	if err != nil {
		return d, nil, nil, nil, err
	}
	subs, err := s.Store.Subagents(r.Context(), id)
	if err != nil {
		return d, nil, nil, nil, err
	}
	return d, msgs, web.ToolsByOrdinal(tools), subs, nil
}

func (s *Server) handleSessionPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		render(w, r, http.StatusNotFound, web.ErrorPage(s.pageFor(r, "Not found"), http.StatusNotFound, "Session not found."))
		return
	}
	d, msgs, tools, subs, err := s.sessionView(r, id)
	if errors.Is(err, store.ErrNotFound) {
		render(w, r, http.StatusNotFound, web.ErrorPage(s.pageFor(r, "Not found"), http.StatusNotFound, "Session not found."))
		return
	}
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not load session."))
		return
	}
	title := fmt.Sprintf("Session #%d", d.ID)
	render(w, r, http.StatusOK, web.SessionPage(s.pageFor(r, title), d, msgs, tools, subs, true))
}

// handleSessionBody serves just the live-updating body fragment, re-fetched by
// the page over SSE when new bytes are parsed.
func (s *Server) handleSessionBody(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d, msgs, tools, subs, err := s.sessionView(r, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	render(w, r, http.StatusOK, web.SessionBody(d, msgs, tools, subs))
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

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	rc := http.NewResponseController(w)

	// Each write gets a bounded deadline rather than clearing the deadline for the
	// whole stream: a client that stops reading would otherwise block the write
	// forever, so the deferred unsubscribe never runs and the subscription leaks.
	// A short deadline turns a stalled client into a write error, ending the loop.
	write := func(payload string) bool {
		if rc.SetWriteDeadline(time.Now().Add(10*time.Second)) != nil {
			return false
		}
		if _, err := fmt.Fprint(w, payload); err != nil {
			return false
		}
		return rc.Flush() == nil
	}

	ch := s.hub.subscribe(id)
	defer s.hub.unsubscribe(id, ch)

	// An initial comment opens the stream so the browser's EventSource fires open.
	if !write(": connected\n\n") {
		return
	}

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			if !write("event: update\ndata: 1\n\n") {
				return
			}
		case <-keepalive.C:
			if !write(": ping\n\n") {
				return
			}
		}
	}
}

func (s *Server) handleSearchPage(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	var hits []store.SearchHit
	if query != "" {
		var err error
		hits, err = s.Store.Search(r.Context(), query, 0, 50)
		if err != nil {
			render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Search failed."))
			return
		}
	}
	render(w, r, http.StatusOK, web.SearchPage(s.pageFor(r, "Search"), query, hits))
}

func (s *Server) handleAccountPage(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	tokens, err := s.Store.ListAPITokens(r.Context(), p.UserID)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.ErrorPage(s.pageFor(r, "Error"), http.StatusInternalServerError, "Could not load tokens."))
		return
	}
	// Freshly minted secrets are passed once via short-lived flash cookies, then
	// cleared, so a page reload does not keep showing them.
	newToken := readFlash(w, r, "akari_new_token")
	newInvite := readFlash(w, r, "akari_new_invite")
	render(w, r, http.StatusOK, web.AccountPage(s.pageFor(r, "Account"), tokens, newToken, newInvite))
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
	if scope != scopeIngest && scope != scopeFull {
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
	if err == nil {
		_ = s.Store.RevokeAPIToken(r.Context(), p.UserID, id)
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
