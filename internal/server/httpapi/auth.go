package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/store"
)

const (
	cookieName    = "akari_session"
	sessionTTL    = 30 * 24 * time.Hour
	scopeIngest   = "ingest"
	scopeFull     = "full"
)

type principal struct {
	UserID int64
	Scope  string
}

type ctxKey int

const principalKey ctxKey = iota

func principalFrom(ctx context.Context) (principal, bool) {
	p, ok := ctx.Value(principalKey).(principal)
	return p, ok
}

// resolve authenticates a request from a Bearer token or a session cookie. A
// cookie always carries full scope; a token carries its stored scope.
func (s *Server) resolve(r *http.Request) (principal, bool) {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		token := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		uid, scope, err := s.Store.TokenAuth(r.Context(), auth.HashToken(token))
		if err != nil {
			return principal{}, false
		}
		return principal{UserID: uid, Scope: scope}, true
	}
	if c, err := r.Cookie(cookieName); err == nil {
		uid, err := s.Store.WebSession(r.Context(), auth.HashToken(c.Value))
		if err == nil {
			return principal{UserID: uid, Scope: scopeFull}, true
		}
	}
	return principal{}, false
}

func (s *Server) withPrincipal(r *http.Request, p principal) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), principalKey, p))
}

// requireIngest accepts any valid credential (ingest or full scope).
func (s *Server) requireIngest(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.resolve(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next(w, s.withPrincipal(r, p))
	}
}

// requireFull demands a full-scope credential (browser session or full token).
func (s *Server) requireFull(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.resolve(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if p.Scope != scopeFull {
			writeError(w, http.StatusForbidden, "full-scope credential required")
			return
		}
		next(w, s.withPrincipal(r, p))
	}
}

// requireAdmin demands a full-scope credential owned by an admin.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return s.requireFull(func(w http.ResponseWriter, r *http.Request) {
		p, _ := principalFrom(r.Context())
		u, err := s.Store.UserByID(r.Context(), p.UserID)
		if err != nil || !u.IsAdmin {
			writeError(w, http.StatusForbidden, "admin required")
			return
		}
		next(w, r)
	})
}

func (s *Server) setSessionCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.Cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(sessionTTL),
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   s.Cfg.CookieSecure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// startSession creates a web session row and sets the cookie. The cookie holds
// the raw secret; only its hash is stored, so a database read cannot recover a
// usable session (matching how API and invite tokens are handled).
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	secret, err := auth.NewToken()
	if err != nil {
		return err
	}
	if err := s.Store.CreateWebSession(r.Context(), auth.HashToken(secret), userID, time.Now().Add(sessionTTL)); err != nil {
		return err
	}
	s.setSessionCookie(w, secret)
	return nil
}

type registerRequest struct {
	Username    string `json:"username"`
	Password    string `json:"password"`
	InviteToken string `json:"invite_token"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, "username and password are required")
		return
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "hash password")
		return
	}
	inviteHash := ""
	if req.InviteToken != "" {
		inviteHash = auth.HashToken(req.InviteToken)
	}
	u, err := s.Store.Register(r.Context(), req.Username, hash, inviteHash)
	switch {
	case errors.Is(err, store.ErrInvalidInvite):
		writeError(w, http.StatusForbidden, "a valid invite token is required to register")
		return
	case isUniqueViolation(err):
		writeError(w, http.StatusConflict, "username already taken")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "create account")
		return
	}
	if err := s.startSession(w, r, u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "start session")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": u.ID, "username": u.Username, "is_admin": u.IsAdmin,
	})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	u, err := s.Store.UserByUsername(r.Context(), strings.TrimSpace(req.Username))
	if err != nil {
		// Do not distinguish unknown user from bad password.
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	ok, err := auth.VerifyPassword(req.Password, u.PasswordHash)
	if err != nil || !ok {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	if err := s.startSession(w, r, u.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "start session")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"username": u.Username, "is_admin": u.IsAdmin})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		_ = s.Store.DeleteWebSession(r.Context(), auth.HashToken(c.Value))
	}
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

type createTokenRequest struct {
	Name  string `json:"name"`
	Scope string `json:"scope"`
}

func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req createTokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Scope == "" {
		req.Scope = scopeIngest
	}
	if req.Scope != scopeIngest && req.Scope != scopeFull {
		writeError(w, http.StatusBadRequest, "scope must be 'ingest' or 'full'")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "token name is required")
		return
	}
	token, err := auth.NewToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate token")
		return
	}
	id, err := s.Store.CreateAPIToken(r.Context(), p.UserID, req.Name, req.Scope, auth.HashToken(token))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store token")
		return
	}
	// The plaintext token is returned exactly once, here.
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "name": req.Name, "scope": req.Scope, "token": token,
	})
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	tokens, err := s.Store.ListAPITokens(r.Context(), p.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list tokens")
		return
	}
	out := make([]map[string]any, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, map[string]any{
			"id": t.ID, "name": t.Name, "scope": t.Scope,
			"created_at": t.CreatedAt, "last_used_at": t.LastUsedAt, "revoked_at": t.RevokedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid token id")
		return
	}
	if err := s.Store.RevokeAPIToken(r.Context(), p.UserID, id); err != nil {
		writeError(w, http.StatusInternalServerError, "revoke token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

type createInviteRequest struct {
	Note         string `json:"note"`
	ExpiresHours int    `json:"expires_hours"`
}

func (s *Server) handleCreateInvite(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	var req createInviteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	token, err := auth.NewToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate invite")
		return
	}
	var expires *time.Time
	if req.ExpiresHours > 0 {
		t := time.Now().Add(time.Duration(req.ExpiresHours) * time.Hour)
		expires = &t
	}
	id, err := s.Store.CreateInvite(r.Context(), auth.HashToken(token), p.UserID, req.Note, expires)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store invite")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "note": req.Note, "invite_token": token, "expires_at": expires,
	})
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
