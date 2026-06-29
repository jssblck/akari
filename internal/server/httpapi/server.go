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
	hub   *sseHub
}

// New builds a Server.
func New(st *store.Store, cfg config.Server) *Server {
	return &Server{Store: st, Cfg: cfg, hub: newSSEHub()}
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

	// Client-side CAS upload: the client lifts tool bodies out of the transcript
	// and uploads them here before sending the transcript that references them.
	mux.HandleFunc("POST /api/v1/ingest/blobs/check", s.requireIngest(s.handleBlobCheck))
	mux.HandleFunc("PUT /api/v1/ingest/blob/{sha256}", s.requireIngest(s.handleBlobUpload))

	// Static assets.
	mux.Handle("GET /static/", staticHandler())

	// CAS blob serving, gated by the referencing session.
	mux.HandleFunc("GET /api/v1/session/{id}/blob/{sha256}", s.requireFull(s.handleSessionBlob))
	mux.HandleFunc("GET /s/{public_id}/blob/{sha256}", s.handlePublicBlob)

	// Server-rendered UI: public, logged-out pages.
	mux.HandleFunc("GET /s/{public_id}", s.handlePublicSession)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLoginForm)
	mux.HandleFunc("GET /register", s.handleRegisterPage)
	mux.HandleFunc("POST /register", s.handleRegisterForm)
	mux.HandleFunc("POST /logout", s.handleLogoutForm)

	// Server-rendered UI: read pages (require a full-scope credential).
	mux.HandleFunc("GET /{$}", s.requireReadHTML(s.handleOverview))
	mux.HandleFunc("GET /projects", s.requireReadHTML(s.handleProjectsIndex))
	mux.HandleFunc("GET /sessions", s.requireReadHTML(s.handleSessions))
	mux.HandleFunc("GET /projects/{id}", s.requireReadHTML(s.handleProjectPage))
	mux.HandleFunc("GET /sessions/{id}", s.requireReadHTML(s.handleSessionPage))
	mux.HandleFunc("GET /sessions/{id}/body", s.requireReadHTML(s.handleSessionBody))
	mux.HandleFunc("GET /sessions/{id}/events", s.requireReadHTML(s.handleSessionEvents))
	mux.HandleFunc("POST /sessions/{id}/publish", s.requireFull(s.handlePublishSession))
	mux.HandleFunc("POST /sessions/{id}/unpublish", s.requireFull(s.handleUnpublishSession))
	mux.HandleFunc("POST /sessions/{id}/delete", s.requireFull(s.handleDeleteSession))
	mux.HandleFunc("GET /account", s.requireReadHTML(s.handleAccountPage))
	mux.HandleFunc("POST /account/tokens", s.requireFull(s.handleCreateTokenForm))
	mux.HandleFunc("POST /account/tokens/{id}/revoke", s.requireFull(s.handleRevokeTokenForm))
	mux.HandleFunc("POST /account/invites", s.requireAdmin(s.handleCreateInviteForm))

	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Pool.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
