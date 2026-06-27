// Package httpapi wires akari-server's HTTP surface: authentication, account and
// token management, and the session ingest protocol.
package httpapi

import (
	"net/http"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/store"
)

// Server holds the dependencies shared by all handlers.
type Server struct {
	Store *store.Store
	Cfg   config.Server
}

// New builds a Server.
func New(st *store.Store, cfg config.Server) *Server {
	return &Server{Store: st, Cfg: cfg}
}

// Routes returns the HTTP handler for the whole API.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Auth and accounts.
	mux.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)

	mux.HandleFunc("GET /api/v1/tokens", s.requireFull(s.handleListTokens))
	mux.HandleFunc("POST /api/v1/tokens", s.requireFull(s.handleCreateToken))
	mux.HandleFunc("POST /api/v1/tokens/{id}/revoke", s.requireFull(s.handleRevokeToken))

	mux.HandleFunc("POST /api/v1/invites", s.requireAdmin(s.handleCreateInvite))

	// Ingest.
	mux.HandleFunc("POST /api/v1/ingest/session", s.requireIngest(s.handleAnnounce))
	mux.HandleFunc("POST /api/v1/ingest/session/{id}/chunk", s.requireIngest(s.handleChunk))
	mux.HandleFunc("POST /api/v1/ingest/session/{id}/reset", s.requireIngest(s.handleReset))

	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Pool.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
