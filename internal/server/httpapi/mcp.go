package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/mcpserver"
	"github.com/jssblck/akari/internal/server/store"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpSessionTimeout reaps an idle MCP session. The Streamable-HTTP transport keeps
// a ServerSession (and its read-loop goroutine and map entry) per initialized
// client until a clean DELETE or close. A coding-agent process that crashes or
// loses the network never sends that, so without a timeout the session, goroutine,
// and map entry would be retained for the life of the server. The timeout closes
// idle sessions; a client that returns simply re-initializes.
const mcpSessionTimeout = 30 * time.Minute

// newMCPHandler builds the Streamable-HTTP handler for the remote MCP server. One
// MCP server instance, holding the read tools, serves every session; the calling
// user is carried per request on the bearer token, not on the server.
//
// DisableLocalhostProtection is deliberate. The go-sdk auto-enables a DNS-rebinding
// guard whenever the accepted connection's local address is loopback: it then 403s
// any request whose Host header is not itself loopback. akari runs as a remote
// server behind a TLS-terminating reverse proxy (Caddy) that dials the app over
// loopback while forwarding the public Host (akari.jessica.black), so that guard
// would reject every authenticated /mcp request. Because the reject only fires once
// a bearer token has passed (the auth middleware runs first, so an unauthenticated
// probe never reaches it), the symptom is that OAuth completes and then the first
// real request 403s, which an MCP client reports as its freshly minted credentials
// being rejected on reconnect. The guard targets locally bound servers a browser
// could reach by rebinding DNS; akari's /mcp is neither browser-reachable without a
// bearer token nor bound to a name an attacker controls, so turning it off is the
// documented choice for a proxied deployment and costs no real protection.
func newMCPHandler(s *Server) http.Handler {
	srv := mcpserver.New(s.Store, mcpserver.Options{ResponseBudgetBytes: s.Cfg.MCPResponseBudgetBytes})
	return mcpsdk.NewStreamableHTTPHandler(
		func(*http.Request) *mcpsdk.Server { return srv },
		&mcpsdk.StreamableHTTPOptions{
			SessionTimeout:             mcpSessionTimeout,
			DisableLocalhostProtection: true,
		},
	)
}

// handleMCP serves the MCP endpoint behind a bearer check. The check is built per
// request so the WWW-Authenticate challenge advertises this origin's protected
// resource metadata even when the public URL is derived from the request rather
// than configured. On success the verified user rides the request context down to
// the tool handlers as TokenInfo.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	setPrivateNoStore(w)
	mw := mcpauth.RequireBearerToken(s.verifyMCPToken, &mcpauth.RequireBearerTokenOptions{
		ResourceMetadataURL: s.baseURL(r) + resourceMetaPath,
	})
	mw(s.mcp).ServeHTTP(w, r)
}

// verifyMCPToken resolves a presented bearer token to the akari user it
// authenticates, for the MCP endpoint. It accepts an OAuth access token minted
// through the consent flow, or a manually created read- or full-scope API token
// (so a non-browser harness can use a token from the account page directly).
// Ingest-only tokens are rejected: ingest is push, not read. The returned UserID is
// the akari user id as a string, which the tool handlers parse back to scope their
// reads.
func (s *Server) verifyMCPToken(ctx context.Context, token string, _ *http.Request) (*mcpauth.TokenInfo, error) {
	hash := auth.HashToken(token)

	// An OAuth access token minted through the consent flow. Only a clean miss
	// (ErrNotFound: not an OAuth token, or expired/revoked) falls through to the API
	// token check; a database or context failure is a real backend error and must not
	// masquerade as an invalid token, so it is wrapped and surfaces as 500.
	uid, scope, expiresAt, err := s.Store.OAuthAccessAuth(ctx, hash)
	switch {
	case err == nil:
		return &mcpauth.TokenInfo{UserID: strconv.FormatInt(uid, 10), Scopes: []string{scope}, Expiration: expiresAt}, nil
	case !errors.Is(err, store.ErrNotFound):
		return nil, fmt.Errorf("resolve oauth access token: %w", err)
	}

	// A manually created read- or full-scope API token (so a non-browser harness can
	// use a token from the account page directly). Ingest-only tokens are rejected:
	// ingest is push, not read.
	uid, scope, err = s.Store.TokenAuth(ctx, hash)
	switch {
	case err == nil:
		if scope != scopeRead && scope != scopeFull {
			return nil, mcpauth.ErrInvalidToken
		}
		// An API token does not expire; the bearer middleware requires a non-zero
		// expiration, so present a rolling one. Revocation still takes effect at once
		// because this verifier hits the database on every request.
		return &mcpauth.TokenInfo{UserID: strconv.FormatInt(uid, 10), Scopes: []string{scope}, Expiration: time.Now().Add(sessionTTL)}, nil
	case errors.Is(err, store.ErrNotFound):
		return nil, mcpauth.ErrInvalidToken
	default:
		return nil, fmt.Errorf("resolve api token: %w", err)
	}
}
