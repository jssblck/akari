package mcpserver_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/jssblck/akari/internal/server/mcpserver"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// seeded names one session's worth of fixture data so the assertions can refer to
// the ids the seed produced.
type seeded struct {
	projectID int64
	sessionID int64
	inputSHA  string
	toolBody  string
	rawBytes  string
}

// seedSession writes one project and one session with a single assistant message,
// a tool call whose input body lives in the CAS, and the raw bytes the session was
// "ingested" from, all by direct insert so the read tools have known data to return
// without driving the whole ingest and parse pipeline.
func seedSession(t *testing.T, st *store.Store) seeded {
	t.Helper()
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	var sessionID int64
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine, cwd, git_branch,
		   message_count, user_message_count)
		 VALUES ($1,$2,'claude','src-1','box','/home/grace/akari','main',2,1) RETURNING id`,
		u.ID, projectID).Scan(&sessionID); err != nil {
		t.Fatalf("session: %v", err)
	}

	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO messages (session_id, ordinal, role, content, model, has_tool_use)
		 VALUES ($1,0,'user','count the files',''      ,false),
		        ($1,1,'assistant','running ls','claude-opus-4-8',true)`, sessionID); err != nil {
		t.Fatalf("messages: %v", err)
	}

	toolBody := `{"command":"ls -1","description":"list files"}`
	inputSHA := store.HashString(toolBody)
	if err := st.PutBlob(ctx, inputSHA, "application/json", "application/json", strings.NewReader(toolBody)); err != nil {
		t.Fatalf("put blob: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO tool_calls (session_id, message_ordinal, call_index, tool_name, category, file_path,
		   input_sha256, input_bytes, input_media_type, result_status)
		 VALUES ($1,1,0,'Bash','exec','', $2, $3, 'application/json','ok')`,
		sessionID, inputSHA, len(toolBody)); err != nil {
		t.Fatalf("tool_call: %v", err)
	}

	raw := "{\"type\":\"user\"}\n{\"type\":\"assistant\"}\n"
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_raw (session_id, byte_len, content_sha256) VALUES ($1,$2,$3)`,
		sessionID, len(raw), store.HashString(raw)); err != nil {
		t.Fatalf("session_raw: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO session_raw_chunks (session_id, byte_offset, byte_len, content) VALUES ($1,0,$2,$3)`,
		sessionID, len(raw), []byte(raw)); err != nil {
		t.Fatalf("session_raw_chunks: %v", err)
	}

	return seeded{projectID: projectID, sessionID: sessionID, inputSHA: inputSHA, toolBody: toolBody, rawBytes: raw}
}

// connect builds the MCP server over an in-memory transport and returns a connected
// client session. The data tools do not read the caller id, so no bearer token is
// needed here; the token path is covered by the httpapi integration test.
func connect(t *testing.T, st *store.Store) *mcpsdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverT, clientT := mcpsdk.NewInMemoryTransports()
	srv := mcpserver.New(st)
	if _, err := srv.Connect(ctx, serverT, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	sess, err := client.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func callJSON(t *testing.T, sess *mcpsdk.ClientSession, name string, args any) map[string]any {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("call %s is error: %+v", name, res.Content)
	}
	text, ok := res.Content[0].(*mcpsdk.TextContent)
	if !ok {
		t.Fatalf("call %s: content %T not text", name, res.Content[0])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
		t.Fatalf("call %s: unmarshal %q: %v", name, text.Text, err)
	}
	return out
}

func TestToolsReturnSeededData(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	fx := seedSession(t, st)
	sess := connect(t, st)

	// list_projects surfaces the one project with its session count.
	projects := callJSON(t, sess, "list_projects", map[string]any{})
	ps, _ := projects["projects"].([]any)
	if len(ps) != 1 {
		t.Fatalf("list_projects: want 1, got %d (%+v)", len(ps), projects)
	}
	p0 := ps[0].(map[string]any)
	if p0["display_name"] != "akari" || int(p0["session_count"].(float64)) != 1 {
		t.Fatalf("project row wrong: %+v", p0)
	}

	// get_session returns the transcript window with the tool call and its input hash.
	sessionDetail := callJSON(t, sess, "get_session", map[string]any{"session_id": fx.sessionID})
	if sessionDetail["cwd"] != "/home/grace/akari" {
		t.Fatalf("get_session cwd = %v", sessionDetail["cwd"])
	}
	tr, _ := sessionDetail["transcript"].(map[string]any)
	if tr == nil {
		t.Fatalf("get_session returned no transcript: %+v", sessionDetail)
	}
	msgs, _ := tr["messages"].([]any)
	if len(msgs) != 2 || tr["has_more"] != false || int(tr["total_messages"].(float64)) != 2 {
		t.Fatalf("transcript window wrong: %+v", tr)
	}
	calls, _ := tr["tool_calls"].([]any)
	if len(calls) != 1 || calls[0].(map[string]any)["input_sha256"] != fx.inputSHA {
		t.Fatalf("transcript tool_calls wrong: %+v", calls)
	}

	// read_tool_body returns the CAS body as text, gated by the referencing session.
	body := callJSON(t, sess, "read_tool_body", map[string]any{"session_id": fx.sessionID, "sha256": fx.inputSHA})
	if body["encoding"] != "text" || body["content"] != fx.toolBody {
		t.Fatalf("read_tool_body mismatch: %+v", body)
	}

	// A body the session does not reference is refused.
	res, err := sess.CallTool(context.Background(), &mcpsdk.CallToolParams{
		Name: "read_tool_body", Arguments: map[string]any{"session_id": fx.sessionID, "sha256": store.HashString("unrelated")},
	})
	if err != nil {
		t.Fatalf("read_tool_body unrelated: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected an error reading an unreferenced body")
	}

	// get_session_raw returns the lossless bytes behind the projection.
	rawOut := callJSON(t, sess, "get_session_raw", map[string]any{"session_id": fx.sessionID})
	if rawOut["content"] != fx.rawBytes || int(rawOut["total_bytes"].(float64)) != len(fx.rawBytes) {
		t.Fatalf("get_session_raw mismatch: %+v", rawOut)
	}
	if rawOut["truncated"] != false {
		t.Fatalf("get_session_raw should not be truncated: %+v", rawOut)
	}
}

func TestGetProjectOmitsZeroRollups(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	fx := seedSession(t, st)
	sess := connect(t, st)

	out := callJSON(t, sess, "get_project", map[string]any{"project_id": fx.projectID})
	proj, _ := out["project"].(map[string]any)
	if proj == nil || proj["display_name"] != "akari" {
		t.Fatalf("get_project project wrong: %+v", out["project"])
	}
	// The project block is identity only: token and cost totals would be zero here
	// (Store.Project does not roll them up) and so must not appear beside analytics,
	// which is the single source of those figures in this response.
	for _, k := range []string{"tokens", "cost_usd", "session_count"} {
		if _, present := proj[k]; present {
			t.Fatalf("get_project project must not carry rollup field %q: %+v", k, proj)
		}
	}
	if _, ok := out["analytics"].(map[string]any); !ok {
		t.Fatalf("get_project missing analytics: %+v", out)
	}
}

// insertSession adds a session with an explicit age (minutes in the past) so feed
// ordering is deterministic. A larger ageMin is a less recently active session.
func insertSession(t *testing.T, st *store.Store, userID, projectID int64, src string, ageMin int) int64 {
	t.Helper()
	var id int64
	if err := st.Pool.QueryRow(context.Background(),
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine, updated_at)
		 VALUES ($1,$2,'claude',$3,'box', now() - make_interval(mins => $4)) RETURNING id`,
		userID, projectID, src, ageMin).Scan(&id); err != nil {
		t.Fatalf("insert session: %v", err)
	}
	return id
}

func TestListSessionsCursorPaging(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	pid, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	for i := 0; i < 5; i++ {
		insertSession(t, st, u.ID, pid, "s"+strconv.Itoa(i), i) // ages 0..4: id order matches recency
	}
	sess := connect(t, st)

	seen := map[float64]bool{}
	cursor := ""
	pages := 0
	for {
		args := map[string]any{"limit": 2}
		if cursor != "" {
			args["cursor"] = cursor
		}
		out := callJSON(t, sess, "list_sessions", args)
		rows, _ := out["sessions"].([]any)
		for _, r := range rows {
			id := r.(map[string]any)["id"].(float64)
			if seen[id] {
				t.Fatalf("session %v returned twice across pages", id)
			}
			seen[id] = true
		}
		pages++
		next, _ := out["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
	}
	if len(seen) != 5 {
		t.Fatalf("cursor paging saw %d sessions, want 5", len(seen))
	}
}

func TestGetSessionTranscriptWindowPaging(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	fx := seedSession(t, st) // 2 messages
	sess := connect(t, st)

	first := callJSON(t, sess, "get_session", map[string]any{
		"session_id": fx.sessionID, "transcript_limit": 1,
	})
	tr := first["transcript"].(map[string]any)
	if int(tr["returned"].(float64)) != 1 || tr["has_more"] != true {
		t.Fatalf("first window: %+v", tr)
	}
	// The first view carries the session-wide duplicate-id aggregate.
	if _, present := first["duplicate_tool_call_ids"]; !present {
		t.Fatalf("first view should carry duplicate_tool_call_ids: %+v", first)
	}
	nextAfter, ok := tr["next_after"].(float64)
	if !ok {
		t.Fatalf("first window missing next_after: %+v", tr)
	}

	second := callJSON(t, sess, "get_session", map[string]any{
		"session_id": fx.sessionID, "transcript_after": int(nextAfter), "transcript_limit": 1,
	})
	tr2 := second["transcript"].(map[string]any)
	if int(tr2["returned"].(float64)) != 1 || tr2["has_more"] != false {
		t.Fatalf("second window: %+v", tr2)
	}
	if _, present := tr2["next_after"]; present {
		t.Fatalf("last window should omit next_after: %+v", tr2)
	}
	// A later page omits the per-session aggregate so paging does not recompute it.
	if _, present := second["duplicate_tool_call_ids"]; present {
		t.Fatalf("paged view should omit duplicate_tool_call_ids: %+v", second)
	}

	// include_transcript=false omits the window entirely but still reports the aggregate.
	off := false
	none := callJSON(t, sess, "get_session", map[string]any{"session_id": fx.sessionID, "include_transcript": off})
	if _, present := none["transcript"]; present {
		t.Fatalf("include_transcript=false should omit transcript: %+v", none)
	}
	if _, present := none["duplicate_tool_call_ids"]; !present {
		t.Fatalf("header-only view should carry duplicate_tool_call_ids: %+v", none)
	}
}

func TestReadToolBodyTruncates(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	fx := seedSession(t, st)
	sess := connect(t, st)

	// A small max_bytes returns only that prefix and flags truncation, while byte_len
	// still reports the full body size. The store reads only the prefix from the CAS,
	// so this stays cheap on a bulky body.
	out := callJSON(t, sess, "read_tool_body", map[string]any{
		"session_id": fx.sessionID, "sha256": fx.inputSHA, "max_bytes": 10,
	})
	if out["encoding"] != "text" || out["content"] != fx.toolBody[:10] {
		t.Fatalf("prefix mismatch: %+v", out)
	}
	if out["truncated"] != true {
		t.Fatalf("want truncated=true: %+v", out)
	}
	if int(out["byte_len"].(float64)) != len(fx.toolBody) {
		t.Fatalf("byte_len should report the full body: %+v", out)
	}
}

func TestRawTruncation(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	fx := seedSession(t, st)
	sess := connect(t, st)

	out := callJSON(t, sess, "get_session_raw", map[string]any{"session_id": fx.sessionID, "max_bytes": 5})
	if out["truncated"] != true {
		t.Fatalf("want truncated=true, got %+v", out)
	}
	if int(out["bytes_returned"].(float64)) != 5 {
		t.Fatalf("want 5 bytes returned, got %+v", out["bytes_returned"])
	}
	if int(out["total_bytes"].(float64)) != len(fx.rawBytes) {
		t.Fatalf("total_bytes should report full size: %+v", out)
	}
}
