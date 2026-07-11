// Package mcpserver builds the read-only Model Context Protocol surface akari
// exposes to coding agents. It mirrors what the web UI shows (the overview
// analytics, the projects index, the session feed, a session's full transcript)
// and adds the raw underlying data the UI reaches for on demand (tool-call bodies
// from the CAS, and the lossless bytes a session was ingested from).
//
// The server is transport-agnostic: this package only registers tools against an
// *mcp.Server. The HTTP wiring (the Streamable-HTTP handler and the OAuth bearer
// check that names the calling user) lives in package httpapi, which owns the
// store and the request lifecycle.
package mcpserver

import (
	"errors"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// New builds an MCP server with every akari read tool registered against st. The
// returned server is safe to hand to a Streamable-HTTP handler for every session;
// the per-request user is carried on each call's bearer token, not on the server.
type Options struct {
	ResponseBudgetBytes int
}

func New(st *store.Store, opts Options) *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "akari",
		Version: version.String(),
		Title:   "akari coding-agent session history",
	}, &mcp.ServerOptions{
		Instructions: instructions,
	})
	registerTools(s, st, newResponder(opts.ResponseBudgetBytes))
	registerMessageResources(s, st)
	return s
}

// instructions orients an agent connecting for the first time. It is surfaced by
// the client as server-level guidance, so it names the shape of the data and how
// the tools compose rather than restating each tool's own description.
const instructions = `akari stores the session logs of coding agents (Claude Code, Codex, pi), parsed and priced, grouped by the git project they ran in.

Start with 'overview' for fleet-wide usage, or 'list_projects' to see projects ranked by recent activity. 'list_sessions' is the cross-project feed with agent/project/user/machine filters; pass a filter value verbatim from the facet counts the same tool returns. 'get_session' pages one session's transcript by encoded response bytes: messages, thinking, and tool-call metadata. Follow next_after while has_more is true. An oversized message field carries a preview and an authenticated resource link for the full text. Tool inputs and results are stored by content hash, not inline; fetch a body with 'read_tool_body' using the sha256 from a tool call. 'get_session_raw' returns the lossless bytes the session was ingested from, behind the parsed projection.

Everything is read-only. You see every internal session, the same surface a logged-in user sees.`

// callerID returns the authenticated user id carried on the call's bearer token.
// The HTTP bearer check rejects an unauthenticated request before any tool runs, so
// a missing or unparsable id here is an internal wiring error, not an auth failure.
func callerID(req *mcp.CallToolRequest) (int64, error) {
	if req == nil {
		return 0, errors.New("no authenticated principal on request")
	}
	return callerIDFromExtra(req.Extra)
}

// mapNotFound turns the store's ErrNotFound into a stable message for tool callers,
// leaving every other error to surface verbatim.
func mapNotFound(err error, what string) error {
	if errors.Is(err, store.ErrNotFound) {
		return errors.New(what + " not found")
	}
	return err
}
