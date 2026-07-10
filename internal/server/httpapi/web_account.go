package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/web"
)

func (s *Server) handleAccountPage(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	tokens, err := s.Store.ListAPITokens(r.Context(), p.UserID)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Could not load tokens.")
		return
	}
	grants, err := s.Store.ListOAuthGrants(r.Context(), p.UserID)
	if err != nil {
		s.renderError(w, r, http.StatusInternalServerError, "Could not load connected apps.")
		return
	}
	page := s.pageForNav(r, "Account", "account")
	// Invites are admin-only machinery: skip the query entirely for a non-admin
	// viewer rather than loading a list the page never renders.
	var invites []store.Invite
	if page.IsAdmin {
		invites, err = s.Store.ListInvites(r.Context())
		if err != nil {
			s.renderError(w, r, http.StatusInternalServerError, "Could not load invites.")
			return
		}
	}
	// Freshly minted secrets are passed once via short-lived flash cookies, then
	// cleared, so a page reload does not keep showing them.
	newToken := readFlash(w, r, "akari_new_token")
	newInvite := readFlash(w, r, "akari_new_invite")
	st := s.worker.FleetStatus(r.Context())
	rp := web.ReparseView{InProgress: st.InProgress, Done: st.Done, Total: st.Total, Failed: st.Failed}
	render(w, r, http.StatusOK, web.AccountPage(page, tokens, grants, invites, newToken, newInvite, rp))
}

// Login and register, form (HTML) variants. These mirror the JSON handlers but
// set the session cookie and redirect instead of returning JSON.

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	setPrivateNoStore(w)
	if p, ok := s.resolve(r); ok && p.Scope == scopeFull {
		http.Redirect(w, r, overviewPath, http.StatusSeeOther)
		return
	}
	next := safeNext(r.URL.Query().Get("next"))
	render(w, r, http.StatusOK, web.LoginPage(web.Page{Title: "Log in"}, next, ""))
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	setPrivateNoStore(w)
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
	setPrivateNoStore(w)
	render(w, r, http.StatusOK, web.RegisterPage(web.Page{Title: "Register"}, ""))
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
	hash, err := auth.HashPassword(password)
	if err != nil {
		render(w, r, http.StatusInternalServerError, web.RegisterPage(web.Page{Title: "Register"}, "Could not create account."))
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
	http.Redirect(w, r, overviewPath, http.StatusSeeOther)
}

func (s *Server) handleLogoutForm(w http.ResponseWriter, r *http.Request) {
	setPrivateNoStore(w)
	var deleteErr error
	if c, err := r.Cookie(cookieName); err == nil {
		deleteErr = s.Store.DeleteWebSession(r.Context(), auth.HashToken(c.Value))
	}
	s.clearSessionCookie(w)
	if deleteErr != nil {
		renderPublicError(w, r, http.StatusInternalServerError, "Could not sign out.")
		return
	}
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
	if !isValidScope(scope) {
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
		s.renderError(w, r, http.StatusInternalServerError, "Could not revoke the token. Try again.")
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
			s.renderError(w, r, http.StatusInternalServerError, "Could not disconnect the app. Try again.")
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
		s.renderError(w, r, http.StatusInternalServerError, "Could not revoke the invite. Try again.")
		return
	}
	s.setNotice(w, "Invite revoked")
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}
