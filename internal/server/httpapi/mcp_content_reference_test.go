package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/config"
	"github.com/jssblck/akari/internal/server/auth"
	"github.com/jssblck/akari/internal/server/parse"
	"github.com/jssblck/akari/internal/server/storetest"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPMessageReferenceRequiresLiveCredential(t *testing.T) {
	t.Parallel()
	const budget = 16 << 10
	ctx := context.Background()
	st := storetest.NewStore(t)
	worker := parse.NewWorker(st, 1, 0)
	srv := httptest.NewServer(New(st, config.Server{MCPResponseBudgetBytes: budget}, worker).Routes())
	t.Cleanup(srv.Close)

	u, err := st.Register(ctx, "grace", mustHash(t, "hopper-1906"), "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	secret, err := auth.NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	tokenID, err := st.CreateAPIToken(ctx, u.ID, "reference reader", "read", auth.HashToken(secret))
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	var sessionID int64
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine, message_count)
		 VALUES ($1,$2,'codex','oversized-reference','box',1) RETURNING id`,
		u.ID, projectID,
	).Scan(&sessionID); err != nil {
		t.Fatalf("session: %v", err)
	}
	full := strings.Repeat("Ada Lovelace <&>\n", 5000)
	if _, err := st.Pool.Exec(ctx,
		"INSERT INTO messages (session_id, ordinal, role, content) VALUES ($1,0,'assistant',$2)",
		sessionID, full,
	); err != nil {
		t.Fatalf("message: %v", err)
	}

	sess := mcpSession(t, srv.URL, secret)
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := sess.CallTool(callCtx, &mcpsdk.CallToolParams{
		Name: "get_session", Arguments: map[string]any{"session_id": sessionID},
	})
	if err != nil {
		t.Fatalf("get_session: %v", err)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structuredContent: %v", err)
	}
	var out struct {
		Transcript struct {
			Messages []struct {
				ContentReference struct {
					URI string `json:"uri"`
				} `json:"content_reference"`
			} `json:"messages"`
		} `json:"transcript"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("decode structuredContent: %v", err)
	}
	if len(out.Transcript.Messages) != 1 || out.Transcript.Messages[0].ContentReference.URI == "" {
		t.Fatalf("missing content reference: %s", b)
	}
	uri := out.Transcript.Messages[0].ContentReference.URI
	resource, err := sess.ReadResource(callCtx, &mcpsdk.ReadResourceParams{URI: uri})
	if err != nil {
		t.Fatalf("read referenced content: %v", err)
	}
	if len(resource.Contents) != 1 || resource.Contents[0].Text != full {
		t.Fatalf("referenced content did not round-trip: contents=%d", len(resource.Contents))
	}
	if _, err := st.Pool.Exec(ctx,
		"UPDATE messages SET content = $3 WHERE session_id = $1 AND ordinal = $2",
		sessionID, 0, full+"changed",
	); err != nil {
		t.Fatalf("change referenced content: %v", err)
	}
	if _, err := sess.ReadResource(callCtx, &mcpsdk.ReadResourceParams{URI: uri}); err == nil {
		t.Fatal("hash-bound reference resolved after its message content changed")
	}
	if _, err := st.Pool.Exec(ctx,
		"UPDATE messages SET content = $3 WHERE session_id = $1 AND ordinal = $2",
		sessionID, 0, full,
	); err != nil {
		t.Fatalf("restore referenced content: %v", err)
	}

	if err := st.RevokeAPIToken(ctx, u.ID, tokenID); err != nil {
		t.Fatalf("revoke token: %v", err)
	}
	revokedCtx, revokedCancel := context.WithTimeout(ctx, 10*time.Second)
	defer revokedCancel()
	if _, err := sess.ReadResource(revokedCtx, &mcpsdk.ReadResourceParams{URI: uri}); err == nil {
		t.Fatal("revoked credential read a previously issued content reference")
	}
}
