// Package httpapi wires akari-server's HTTP surface: authentication, account and
// token management, and the session ingest protocol.
package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/reparse"
	"github.com/jssblck/akari/internal/server/store"
)

// Server holds the dependencies shared by all handlers.
type Server struct {
	Store    *store.Store
	Cfg      config.Server
	hub      *sseHub
	reparser *reparse.Service
}

// New builds a Server. The reparse service is shared with the startup auto-run and
// the CLI; here it backs the admin Reparse button, the status endpoint, and the UI
// gating, and its progress is pushed to watching browsers over the SSE hub.
func New(st *store.Store, cfg config.Server, reparser *reparse.Service) *Server {
	s := &Server{Store: st, Cfg: cfg, hub: newSSEHub(), reparser: reparser}
	// Fan reparse progress out to any browser watching the status stream. The hub
	// carries the status JSON as the payload, so a watcher updates its progress bar
	// directly rather than round-tripping to the status endpoint.
	reparser.SetProgressHook(func(status reparse.Status) {
		if b, err := json.Marshal(status); err == nil {
			s.hub.publishReparse(string(b))
		}
	})
	return s
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

	// CAS blob serving, gated by the referencing session. Raw blob bytes stay
	// available during a reparse (they are content-addressed and not part of the
	// parsed projection), so these are not behind the reparse gate.
	mux.HandleFunc("GET /api/v1/session/{id}/blob/{sha256}", s.requireFull(s.handleSessionBlob))
	mux.HandleFunc("GET /s/{public_id}/blob/{sha256}", s.handlePublicBlob)

	// Reparse status and live progress. The status JSON is the poll fallback; the
	// SSE stream pushes the same payload so a watching page updates its progress bar
	// without polling. Both require a full-scope credential.
	mux.HandleFunc("GET /api/v1/reparse/status", s.requireFull(s.handleReparseStatus))
	mux.HandleFunc("GET /api/v1/reparse/events", s.requireFull(s.handleReparseEvents))

	// Server-rendered UI: public, logged-out pages. The public session view shows
	// parsed data, so it is gated while a reparse rebuilds the projection.
	mux.HandleFunc("GET /s/{public_id}", s.gatePublicParsed(s.handlePublicSession))
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLoginForm)
	mux.HandleFunc("GET /register", s.handleRegisterPage)
	mux.HandleFunc("POST /register", s.handleRegisterForm)
	mux.HandleFunc("POST /logout", s.handleLogoutForm)

	// Server-rendered UI: read pages (require a full-scope credential). Pages that
	// serve parsed/projected session data are wrapped in gateParsed, which renders a
	// "reparse in progress" page (with a live progress bar) instead of stale or
	// half-rebuilt rows while a reparse runs. The session events SSE stream stays
	// ungated: gating it would write HTML into the event stream, and the gated
	// session page does not open it anyway.
	mux.HandleFunc("GET /{$}", s.requireReadHTML(s.gateParsed(s.handleOverview)))
	mux.HandleFunc("GET /projects", s.requireReadHTML(s.gateParsed(s.handleProjectsIndex)))
	mux.HandleFunc("GET /sessions", s.requireReadHTML(s.gateParsed(s.handleSessions)))
	mux.HandleFunc("GET /projects/{id}", s.requireReadHTML(s.gateParsed(s.handleProjectPage)))
	mux.HandleFunc("GET /sessions/{id}", s.requireReadHTML(s.gateParsed(s.handleSessionPage)))
	mux.HandleFunc("GET /sessions/{id}/body", s.requireReadHTML(s.gateParsed(s.handleSessionBody)))
	mux.HandleFunc("GET /sessions/{id}/events", s.requireReadHTML(s.handleSessionEvents))
	mux.HandleFunc("POST /sessions/{id}/publish", s.requireFull(s.handlePublishSession))
	mux.HandleFunc("POST /sessions/{id}/unpublish", s.requireFull(s.handleUnpublishSession))
	mux.HandleFunc("POST /sessions/{id}/delete", s.requireFull(s.handleDeleteSession))
	mux.HandleFunc("GET /search", s.requireReadHTML(s.gateParsed(s.handleSearchPage)))

	// Account stays fully available during a reparse: it is not parsed data, and it
	// hosts the reparse status and the admin Reparse button.
	mux.HandleFunc("GET /account", s.requireReadHTML(s.handleAccountPage))
	mux.HandleFunc("POST /account/tokens", s.requireFull(s.handleCreateTokenForm))
	mux.HandleFunc("POST /account/tokens/{id}/revoke", s.requireFull(s.handleRevokeTokenForm))
	mux.HandleFunc("POST /account/invites", s.requireAdmin(s.handleCreateInviteForm))
	mux.HandleFunc("POST /account/reparse", s.requireAdmin(s.handleReparseForm))

	return mux
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Pool.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
