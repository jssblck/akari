// Package httpapi wires akari-server's HTTP surface: authentication, account and
// token management, and the session ingest protocol.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"golang.org/x/sync/singleflight"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/requestbudget"
	"github.com/jssblck/akari/internal/server/store"
)

// Server holds the dependencies shared by all handlers.
type Server struct {
	Store  *store.Store
	Cfg    config.Server
	hub    *sseHub
	worker *parse.Worker
	budget *requestbudget.Budget
	// mcp is the Streamable-HTTP handler for the remote MCP server, built once and
	// shared across requests; handleMCP wraps it per request with the bearer check.
	mcp http.Handler
	// ogRender, ogProjectRender, and ogSessionRender coalesce concurrent on-demand
	// renders of the same Open Graph card, keyed by the entity id. A crawler burst (or
	// several unfurls landing at once on a cache miss or an expired card) would otherwise
	// run the full render once per request; singleflight lets one render run while the
	// rest wait for and serve its result. They are separate groups (not one shared group
	// with prefixed keys) so a user id and a project id that happen to share a numeric
	// value never collide on one in-flight render.
	ogRender        singleflight.Group
	ogProjectRender singleflight.Group
	ogSessionRender singleflight.Group
	// insights holds the fleet Insights snapshot: every trailing window computed in
	// one store pass, recomputed hourly in the background (and on reparse completion)
	// so the range views always describe one corpus state and every load is a map
	// lookup. See insights_refresh.go.
	insights *insightsRefresher
}

// New builds a Server. The parse worker is shared with the server main loop; here
// it backs the admin Reparse button, the rebuild status endpoint, and the UI
// gating. Its hooks fan out through the SSE hub: fleet-rebuild progress to the
// status stream, and each committed rebuild to the browsers watching that session.
func New(st *store.Store, cfg config.Server, worker *parse.Worker) *Server {
	capacity := int64(cfg.RequestBudgetCapacity)
	if capacity == 0 {
		capacity = requestbudget.DefaultCapacity
	}
	waitTimeout := cfg.RequestBudgetWaitTimeout
	if waitTimeout == 0 {
		waitTimeout = requestbudget.DefaultWaitTimeout
	}
	budget, err := requestbudget.New(capacity, waitTimeout)
	if err != nil {
		panic("invalid request budget configuration: " + err.Error())
	}
	if cfg.OAuthRegistrationsPerHour == 0 {
		cfg.OAuthRegistrationsPerHour = config.DefaultOAuthRegistrationsPerHour
	}
	s := &Server{Store: st, Cfg: cfg, hub: newSSEHub(), worker: worker, budget: budget}
	s.insights = newInsightsRefresher(func(ctx context.Context) (map[string]store.Insights, error) {
		return computeFleetInsights(ctx, st)
	})
	s.mcp = newMCPHandler(s)
	// Fan fleet-rebuild progress out to any browser watching the status stream. The
	// hub carries the status JSON as the payload, so a watcher updates its progress
	// bar directly rather than round-tripping to the status endpoint.
	worker.SetStatusHook(func(status parse.Status) {
		if b, err := json.Marshal(status); err == nil {
			s.hub.publishReparse(string(b))
		}
		// A finished fleet drain just rewrote the corpus under the insights snapshot;
		// recompute now rather than serving pre-reparse figures until the next tick.
		if !status.InProgress {
			s.insights.kickRefresh()
		}
	})
	// Wake the browsers watching a session when its rebuild commits, so the live
	// view refreshes when there is actually a new projection to fetch (the chunk
	// handler only appends raw bytes; parsing is async).
	worker.SetRebuiltHook(func(sessionID int64) {
		s.hub.publish(sessionID)
	})
	return s
}

// Routes returns the HTTP handler for the whole API.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.Handle("GET /metrics", s.budget)

	// Auth and accounts.
	mux.HandleFunc("POST /api/v1/auth/register", s.handleRegister)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", s.handleLogout)

	mux.HandleFunc("GET /api/v1/tokens", s.requireFull(s.handleListTokens))
	mux.HandleFunc("POST /api/v1/tokens", s.requireFull(s.handleCreateToken))
	mux.HandleFunc("POST /api/v1/tokens/{id}/revoke", s.requireFull(s.handleRevokeToken))

	mux.HandleFunc("POST /api/v1/invites", s.requireAdmin(s.handleCreateInvite))

	// Remote MCP server and the OAuth 2.1 authorization surface that guards it. The
	// discovery documents, dynamic client registration, and token endpoint are
	// public; the authorize endpoint recognizes the browser session cookie so a user
	// connects an agent with one consent click. See oauth.go and mcp.go.
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.handleProtectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", s.handleProtectedResourceMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", s.handleAuthServerMetadata)
	mux.HandleFunc("POST /oauth/register", s.admit(requestbudget.OAuthRegistration, s.handleOAuthRegister))
	mux.HandleFunc("GET /oauth/authorize", s.handleOAuthAuthorize)
	mux.HandleFunc("POST /oauth/authorize", s.handleOAuthDecision)
	mux.HandleFunc("POST /oauth/token", s.handleOAuthToken)
	// The MCP transport multiplexes POST (messages), GET (the SSE stream), and
	// DELETE (session teardown) on one path, so it registers without a method filter
	// and authenticates each request itself via the bearer check in handleMCP.
	mux.Handle(mcpPath, s.admitMCP(http.HandlerFunc(s.handleMCP)))

	// Ingest.
	mux.HandleFunc("POST /api/v1/ingest/session", s.requireIngest(s.handleAnnounce))
	mux.HandleFunc("POST /api/v1/ingest/session/{id}/chunk", s.requireIngest(s.handleChunk))
	mux.HandleFunc("POST /api/v1/ingest/session/{id}/reset", s.requireIngest(s.handleReset))
	// Grade a terminal session now (end of `akari sync --finalize`) rather than on the
	// next settle tick, so an ephemeral host sees the grade before it is torn down.
	mux.HandleFunc("POST /api/v1/ingest/session/{id}/finalize", s.requireIngest(s.handleFinalize))

	// Client-side CAS upload: the client lifts tool bodies out of the transcript
	// and uploads them here before sending the transcript that references them.
	mux.HandleFunc("POST /api/v1/ingest/blobs/check", s.requireIngest(s.handleBlobCheck))
	mux.HandleFunc("PUT /api/v1/ingest/blob/{sha256}", s.requireIngest(s.handleBlobUpload))

	// Static assets.
	mux.Handle("GET /static/", staticHandler())
	// Browsers request /favicon.ico at the root unprompted; serve the embedded
	// icon there so that automatic hit does not 404.
	mux.HandleFunc("GET /favicon.ico", s.handleFaviconICO)

	// The user guide: public documentation, readable logged out and served to a
	// coding agent as raw Markdown and as one concatenated file. It is static
	// content independent of the parsed projection, so it carries neither the auth
	// gate nor the reparse gate. handleGuidePage splits the .md suffix itself, so
	// /guide/<slug> and /guide/<slug>.md share one route.
	mux.HandleFunc("GET /guide", s.handleGuideIndex)
	mux.HandleFunc("GET /guide/{slug}", s.handleGuidePage)
	mux.HandleFunc("GET /llms.txt", s.handleLLMsTxt)
	mux.HandleFunc("GET /llms-full.txt", s.handleLLMsFullTxt)

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
	// A user's published usage overview at /u/<username>: aggregate, scoped to that
	// one account, and gated during a reparse like the public session view (it shows
	// parsed data).
	mux.HandleFunc("GET /u/{username}", s.admit(requestbudget.PublicAnalytics, s.gatePublicParsed(s.handlePublicOverview)))
	// A project's published usage overview at /p/<id>: aggregate, scoped to that one
	// project across every account, with no session list. Gated during a reparse like
	// the other public parsed pages.
	mux.HandleFunc("GET /p/{id}", s.admit(requestbudget.PublicAnalytics, s.gatePublicParsed(s.handlePublicProject)))
	// The Open Graph preview cards for the three per-entity public pages. Each serves
	// PNG bytes rendered on demand and held in a TTL cache, so none is reparse-gated: the
	// more specific /og.png pattern wins over the page pattern (/u/{username}, /p/{id},
	// /s/{public_id}) for these exact paths. Unlike the page routes above, these are not
	// wrapped in s.admit: a cache hit or a 404 costs no admission, and a cache miss is
	// charged exactly once per coalesced render by the singleflight leader inside
	// renderCoalesced, not once per request. Wrapping the whole handler here would charge
	// admission for every concurrent unfurl of the same card and for every cache hit, the
	// opposite of what the budget is for.
	mux.HandleFunc("GET /u/{username}/og.png", s.handlePublicOverviewOGImage)
	mux.HandleFunc("GET /p/{id}/og.png", s.handlePublicProjectOGImage)
	mux.HandleFunc("GET /s/{public_id}/og.png", s.handlePublicSessionOGImage)
	// The Open Graph preview card for the instance root ("/"). It serves static PNG
	// bytes memoized per binary (see handleLandingOGImage), so like the overview
	// card route it needs no auth and no reparse gate.
	mux.HandleFunc("GET /og.png", s.handleLandingOGImage)
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
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /overview", s.requireReadHTML(s.gateParsed(s.handleOverview)))
	mux.HandleFunc("GET /insights", s.requireReadHTML(s.gateParsed(s.handleInsights)))
	mux.HandleFunc("GET /projects", s.requireReadHTML(s.gateParsed(s.handleProjectsIndex)))
	mux.HandleFunc("GET /sessions", s.requireReadHTML(s.gateParsed(s.handleSessions)))
	mux.HandleFunc("GET /projects/{id}", s.requireReadHTML(s.gateParsed(s.handleProjectPage)))
	mux.HandleFunc("GET /sessions/{id}", s.requireReadHTML(s.gateParsed(s.handleSessionPage)))
	mux.HandleFunc("GET /sessions/{id}/body", s.requireReadHTML(s.gateParsed(s.handleSessionBody)))
	mux.HandleFunc("GET /sessions/{id}/events", s.requireReadHTML(s.handleSessionEvents))
	mux.HandleFunc("POST /sessions/{id}/publish", s.requireFull(s.handlePublishSession))
	mux.HandleFunc("POST /sessions/{id}/unpublish", s.requireFull(s.handleUnpublishSession))
	mux.HandleFunc("POST /sessions/{id}/delete", s.requireFull(s.handleDeleteSession))
	// A project's overview publicity toggle. Projects are fleet-global, so this needs
	// only a full-scope credential (any signed-in user), not an owner check.
	mux.HandleFunc("POST /projects/{id}/overview/publish", s.requireFull(s.handlePublishProjectOverview))
	mux.HandleFunc("POST /projects/{id}/overview/unpublish", s.requireFull(s.handleUnpublishProjectOverview))

	// Account stays fully available during a reparse: it is not parsed data, and it
	// hosts the reparse status and the admin Reparse button.
	mux.HandleFunc("GET /account", s.requireReadHTML(s.handleAccountPage))
	mux.HandleFunc("POST /account/tokens", s.requireFull(s.handleCreateTokenForm))
	mux.HandleFunc("POST /account/tokens/{id}/revoke", s.requireFull(s.handleRevokeTokenForm))
	mux.HandleFunc("POST /account/connections/{client_id}/revoke", s.requireFull(s.handleRevokeConnectionForm))
	mux.HandleFunc("POST /account/invites", s.requireAdmin(s.handleCreateInviteForm))
	mux.HandleFunc("POST /account/invites/{id}/revoke", s.requireAdmin(s.handleRevokeInviteForm))
	mux.HandleFunc("POST /account/reparse", s.requireAdmin(s.handleReparseForm))
	mux.HandleFunc("POST /account/overview/publish", s.requireFull(s.handlePublishOverview))
	mux.HandleFunc("POST /account/overview/unpublish", s.requireFull(s.handleUnpublishOverview))

	return withStyledNotFound(mux, s.handleNotFound)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Pool.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
