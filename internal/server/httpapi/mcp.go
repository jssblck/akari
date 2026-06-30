package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/mcpserver"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// newMCPHandler builds the Streamable-HTTP handler for the remote MCP server. One
// MCP server instance, holding the read tools, serves every session; the calling
// user is carried per request on the bearer token, not on the server.
func newMCPHandler(s *Server) http.Handler {
	srv := mcpserver.New(s.Store)
	return mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return srv }, nil)
}

// handleMCP serves the MCP endpoint behind a bearer check. The check is built per
// request so the WWW-Authenticate challenge advertises this origin's protected
// resource metadata even when the public URL is derived from the request rather
// than configured. On success the verified user rides the request context down to
// the tool handlers as TokenInfo.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
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
	if uid, scope, expiresAt, err := s.Store.OAuthAccessAuth(ctx, hash); err == nil {
		return &mcpauth.TokenInfo{UserID: strconv.FormatInt(uid, 10), Scopes: []string{scope}, Expiration: expiresAt}, nil
	}
	if uid, scope, err := s.Store.TokenAuth(ctx, hash); err == nil && (scope == scopeRead || scope == scopeFull) {
		// An API token does not expire; the bearer middleware requires a non-zero
		// expiration, so present a rolling one. Revocation still takes effect at once
		// because this verifier hits the database on every request.
		return &mcpauth.TokenInfo{UserID: strconv.FormatInt(uid, 10), Scopes: []string{scope}, Expiration: time.Now().Add(sessionTTL)}, nil
	}
	return nil, mcpauth.ErrInvalidToken
}
