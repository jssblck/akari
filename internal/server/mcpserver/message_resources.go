package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const messageResourceTemplate = "akari://sessions/{session_id}/messages/{ordinal}/{field}/{sha256}"

func registerMessageResources(s *mcp.Server, st *store.Store) {
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: messageResourceTemplate,
		Name:        "session-message-field",
		Description: "Full content or thinking text omitted from a byte-bounded transcript response.",
		MIMEType:    "text/plain; charset=utf-8",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		if _, err := callerIDFromExtra(req.Extra); err != nil {
			return nil, err
		}
		sessionID, ordinal, field, sha256, err := parseMessageResourceURI(req.Params.URI)
		if err != nil {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		text, err := st.MessageText(ctx, sessionID, ordinal, field, sha256)
		if errors.Is(err, store.ErrNotFound) {
			return nil, mcp.ResourceNotFoundError(req.Params.URI)
		}
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI: req.Params.URI, MIMEType: "text/plain; charset=utf-8", Text: text,
		}}}, nil
	})
}

func messageResourceURI(sessionID int64, ordinal int, field, sha256 string) string {
	return fmt.Sprintf("akari://sessions/%d/messages/%d/%s/%s", sessionID, ordinal, field, sha256)
}

func parseMessageResourceURI(raw string) (int64, int, string, string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "akari" || u.Host != "sessions" || u.RawQuery != "" || u.Fragment != "" {
		return 0, 0, "", "", errors.New("invalid message resource URI")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 5 || parts[1] != "messages" || (parts[3] != "content" && parts[3] != "thinking") || !validSHA256(parts[4]) {
		return 0, 0, "", "", errors.New("invalid message resource URI")
	}
	sessionID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || sessionID <= 0 {
		return 0, 0, "", "", errors.New("invalid message resource URI")
	}
	ordinal, err := strconv.Atoi(parts[2])
	if err != nil || ordinal < 0 {
		return 0, 0, "", "", errors.New("invalid message resource URI")
	}
	return sessionID, ordinal, parts[3], parts[4], nil
}

func validSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func callerIDFromExtra(extra *mcp.RequestExtra) (int64, error) {
	if extra == nil || extra.TokenInfo == nil {
		return 0, errors.New("no authenticated principal on request")
	}
	id, err := strconv.ParseInt(extra.TokenInfo.UserID, 10, 64)
	if err != nil {
		return 0, errors.New("authenticated principal has no user id")
	}
	return id, nil
}
