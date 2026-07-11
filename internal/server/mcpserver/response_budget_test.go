package mcpserver_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/mcpserver"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func connectWithBudget(t *testing.T, st *store.Store, budget int) *mcpsdk.ClientSession {
	t.Helper()
	serverT, clientT := mcpsdk.NewInMemoryTransports()
	srv := mcpserver.New(st, mcpserver.Options{ResponseBudgetBytes: budget})
	if _, err := srv.Connect(context.Background(), serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "budget-test", Version: "0"}, nil)
	sess, err := client.Connect(context.Background(), clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func callResult(t *testing.T, sess *mcpsdk.ClientSession, name string, args any) *mcpsdk.CallToolResult {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("call %s returned error: %+v", name, res.Content)
	}
	return res
}

func structuredMap(t *testing.T, res *mcpsdk.CallToolResult) map[string]any {
	t.Helper()
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structuredContent: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal structuredContent: %v", err)
	}
	return out
}

func replaceTranscript(t *testing.T, st *store.Store, sessionID int64, contents []string, thinking string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.Pool.Exec(ctx, "DELETE FROM tool_calls WHERE session_id = $1", sessionID); err != nil {
		t.Fatalf("clear tool calls: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, "DELETE FROM messages WHERE session_id = $1", sessionID); err != nil {
		t.Fatalf("clear messages: %v", err)
	}
	for i, content := range contents {
		if _, err := st.Pool.Exec(ctx,
			`INSERT INTO messages (session_id, ordinal, role, content, thinking_text, has_thinking)
			 VALUES ($1,$2,'assistant',$3,$4,$5)`,
			sessionID, i, content, thinking, thinking != "",
		); err != nil {
			t.Fatalf("insert message %d: %v", i, err)
		}
	}
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET message_count = $2 WHERE id = $1", sessionID, len(contents)); err != nil {
		t.Fatalf("update message count: %v", err)
	}
}

func TestToolResultUsesCompactTextAndStructuredData(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	seedSession(t, st)
	res := callResult(t, connectWithBudget(t, st, 64<<10), "list_projects", map[string]any{})
	if res.StructuredContent == nil {
		t.Fatal("structured client received no structuredContent")
	}
	text, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("text-only client received %T, want TextContent", res.Content[0])
	}
	if !strings.Contains(text.Text, "Full data is in structuredContent") || strings.Contains(text.Text, `"projects"`) {
		t.Fatalf("text summary is not compact: %q", text.Text)
	}
}

func TestTranscriptBudgetCountsJSONEscapingAndReferencesOversizedFields(t *testing.T) {
	t.Parallel()
	const budget = 16 << 10
	st := storetest.NewStore(t)
	fx := seedSession(t, st)
	content := strings.Repeat("\x01", 4000)
	thinking := strings.Repeat("<&", 4000)
	replaceTranscript(t, st, fx.sessionID, []string{content}, thinking)

	res := callResult(t, connectWithBudget(t, st, budget), "get_session", map[string]any{"session_id": fx.sessionID})
	encoded, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if len(encoded) > budget {
		t.Fatalf("encoded result = %d bytes, budget = %d", len(encoded), budget)
	}
	out := structuredMap(t, res)
	tr := out["transcript"].(map[string]any)
	if tr["byte_budget_truncated"] != true || tr["has_more"] != false {
		t.Fatalf("oversized single-message page flags = %+v", tr)
	}
	msg := tr["messages"].([]any)[0].(map[string]any)
	if int(msg["content_byte_len"].(float64)) != len(content) || msg["content_reference"] == nil {
		t.Fatalf("content reference metadata = %+v", msg)
	}
	if int(msg["thinking_text_byte_len"].(float64)) != len(thinking) || msg["thinking_text_reference"] == nil {
		t.Fatalf("thinking reference metadata = %+v", msg)
	}
	if len(msg["content"].(string)) >= len(content) || len(msg["thinking_text"].(string)) >= len(thinking) {
		t.Fatal("oversized fields were returned in full")
	}
	links := 0
	for _, block := range res.Content {
		if _, ok := block.(*mcpsdk.ResourceLink); ok {
			links++
		}
	}
	if links != 2 {
		t.Fatalf("resource links = %d, want 2", links)
	}
}

func TestTranscriptBytePagesRemainOrderedAndLossless(t *testing.T) {
	t.Parallel()
	const budget = 12 << 10
	st := storetest.NewStore(t)
	fx := seedSession(t, st)
	contents := make([]string, 40)
	for i := range contents {
		contents[i] = fmt.Sprintf("message-%02d:%s", i, strings.Repeat(`\"<&`, 180))
	}
	replaceTranscript(t, st, fx.sessionID, contents, "")
	sess := connectWithBudget(t, st, budget)

	after := -1
	seen := make([]int, 0, len(contents))
	for page := 0; ; page++ {
		args := map[string]any{"session_id": fx.sessionID, "transcript_limit": 100}
		if after >= 0 {
			args["transcript_after"] = after
		}
		res := callResult(t, sess, "get_session", args)
		encoded, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("marshal page %d: %v", page, err)
		}
		if len(encoded) > budget {
			t.Fatalf("page %d encoded size = %d, budget = %d", page, len(encoded), budget)
		}
		tr := structuredMap(t, res)["transcript"].(map[string]any)
		for _, raw := range tr["messages"].([]any) {
			seen = append(seen, int(raw.(map[string]any)["ordinal"].(float64)))
		}
		if tr["has_more"] == false {
			break
		}
		if tr["byte_budget_truncated"] != true {
			t.Fatalf("page %d continued without byte_budget_truncated: %+v", page, tr)
		}
		next, ok := tr["next_after"].(float64)
		if !ok {
			t.Fatalf("page %d missing next_after: %+v", page, tr)
		}
		after = int(next)
		if page > len(contents) {
			t.Fatal("pagination did not terminate")
		}
	}
	if len(seen) != len(contents) {
		t.Fatalf("saw %d messages, want %d: %v", len(seen), len(contents), seen)
	}
	for i, ordinal := range seen {
		if ordinal != i {
			t.Fatalf("message %d had ordinal %d", i, ordinal)
		}
	}
}

func TestListSessionsByteBudgetKeepsCursorLossless(t *testing.T) {
	t.Parallel()
	const budget = 8 << 10
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	const total = 24
	for i := 0; i < total; i++ {
		id := insertSession(t, st, u.ID, projectID, fmt.Sprintf("byte-page-%02d", i), i)
		if _, err := st.Pool.Exec(ctx,
			"UPDATE sessions SET git_branch = $2 WHERE id = $1",
			id, fmt.Sprintf("branch-%02d-%s", i, strings.Repeat("x", 900)),
		); err != nil {
			t.Fatalf("set branch %d: %v", i, err)
		}
	}
	sess := connectWithBudget(t, st, budget)
	seen := map[int]bool{}
	cursor := ""
	for page := 0; ; page++ {
		args := map[string]any{"limit": 100}
		if cursor != "" {
			args["cursor"] = cursor
		}
		res := callResult(t, sess, "list_sessions", args)
		encoded, err := json.Marshal(res)
		if err != nil {
			t.Fatalf("marshal page %d: %v", page, err)
		}
		if len(encoded) > budget {
			t.Fatalf("page %d encoded size = %d, budget = %d", page, len(encoded), budget)
		}
		out := structuredMap(t, res)
		rows := out["sessions"].([]any)
		if len(rows) == 0 {
			t.Fatalf("page %d returned no rows", page)
		}
		for _, raw := range rows {
			id := int(raw.(map[string]any)["id"].(float64))
			if seen[id] {
				t.Fatalf("session %d returned twice", id)
			}
			seen[id] = true
		}
		next, _ := out["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
		if page > total {
			t.Fatal("session pagination did not terminate")
		}
	}
	if len(seen) != total {
		t.Fatalf("saw %d sessions, want %d", len(seen), total)
	}
}
