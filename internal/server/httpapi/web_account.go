package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/web"
)

// Login and register, form (HTML) variants. These mirror the JSON handlers but
// set the session cookie and redirect instead of returning JSON.

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	setPrivateNoStore(w)
	if err := r.ParseForm(); err != nil {
		render(w, r, http.StatusBadRequest, web.LoginPage(web.Page{Title: "Log in"}, s.safeNext(r, ""), "Invalid form."))
		return
	}
	next := s.safeNext(r, r.PostFormValue("next"))
	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	u, ok := s.authenticatePassword(r, username, password)
	if !ok {
		render(w, r, http.StatusUnauthorized, web.LoginPage(web.Page{Title: "Log in"}, next, "Invalid credentials."))
		return
	}
	if err := s.startSession(w, r, u.ID); err != nil {
		render(w, r, http.StatusInternalServerError, web.LoginPage(web.Page{Title: "Log in"}, next, "Could not start session."))
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleRegisterForm(w http.ResponseWriter, r *http.Request) {
	setPrivateNoStore(w)
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
	if !s.authAttempts.Allow(username, requestSource(r), time.Now()) {
		renderRegistrationUnavailable(w, r)
		return
	}
	inviteHash := ""
	if invite != "" {
		inviteHash = auth.HashToken(invite)
	}
	if err := s.Store.CheckRegistrationInvite(r.Context(), inviteHash); errors.Is(err, store.ErrInvalidInvite) {
		render(w, r, http.StatusForbidden, web.RegisterPage(web.Page{Title: "Register"}, "A valid invite token is required."))
		return
	} else if err != nil {
		render(w, r, http.StatusInternalServerError, web.RegisterPage(web.Page{Title: "Register"}, "Could not create account."))
		return
	}
	hash, err := s.passwords.Hash(r.Context(), password)
	if err != nil {
		renderRegistrationUnavailable(w, r)
		return
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
	http.Redirect(w, r, s.href(r, overviewPath), http.StatusSeeOther)
}

func renderRegistrationUnavailable(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "1")
	render(w, r, http.StatusServiceUnavailable, web.RegisterPage(web.Page{Title: "Register"}, "Registration is temporarily unavailable. Please try again."))
}

func (s *Server) handleLogoutForm(w http.ResponseWriter, r *http.Request) {
	setPrivateNoStore(w)
	var deleteErr error
	if c, err := r.Cookie(cookieName); err == nil {
		deleteErr = s.Store.DeleteWebSession(r.Context(), auth.HashToken(c.Value))
	}
	s.clearSessionCookie(w, r)
	// Rotate the CSRF cookie too: the prior token must not outlive the session
	// it was issued alongside.
	rotateErr := s.rotateCSRFCookie(w, r)
	if deleteErr != nil || rotateErr != nil {
		renderPublicError(w, r, http.StatusInternalServerError, "Could not sign out.")
		return
	}
	http.Redirect(w, r, s.href(r, "/login"), http.StatusSeeOther)
}
