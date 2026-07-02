package mcpserver_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

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
		`INSERT INTO tool_calls (session_id, message_ordinal, call_index, tool_name, category, file_path, detail,
		   input_sha256, input_bytes, input_media_type, result_status)
		 VALUES ($1,1,0,'Bash','exec','','ls -1', $2, $3, 'application/json','ok')`,
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
	text := rawToolResult(t, sess, name, args)
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("call %s: unmarshal %q: %v", name, text, err)
	}
	return out
}

// rawToolResult returns a tool call's result as the literal JSON text the wire
// carries, for a test that needs to pin an exact field spelling (a key name, a
// value's quoting) rather than the decoded map callJSON produces.
func rawToolResult(t *testing.T, sess *mcpsdk.ClientSession, name string, args any) string {
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
	return text.Text
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
	// The tool call's derived detail (the command summary) rides on the DTO the same
	// way file_path does, so a caller reading the transcript can scan the command
	// without a separate read_tool_body round trip.
	if calls[0].(map[string]any)["detail"] != "ls -1" {
		t.Fatalf("transcript tool_calls missing detail: %+v", calls)
	}
	if raw := rawToolResult(t, sess, "get_session", map[string]any{"session_id": fx.sessionID}); !strings.Contains(raw, `"detail": "ls -1"`) {
		t.Fatalf("get_session transcript JSON missing literal detail field: %s", raw)
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
// ordering is deterministic. A larger ageMin is a less recently active session. The
// age is set on ended_at, which the generated last_active_at column reads and the
// feed orders by, rather than on updated_at (the row-write time the feed no longer
// sorts on since migration 0033).
func insertSession(t *testing.T, st *store.Store, userID, projectID int64, src string, ageMin int) int64 {
	t.Helper()
	var id int64
	if err := st.Pool.QueryRow(context.Background(),
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine, ended_at)
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

// TestListSessionsExposesLastActiveAt pins the MCP session DTO's recency field: it is
// spelled last_active_at (the session's last-event time, which insertSession sets on
// ended_at), and the old updated_at spelling is gone. A client ordering a page by
// recency then reads the same value the web feed shows, not the row-write time a
// reparse restamps.
func TestListSessionsExposesLastActiveAt(t *testing.T) {
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
	insertSession(t, st, u.ID, pid, "s1", 60) // ended_at ~60 minutes ago
	sess := connect(t, st)

	// The literal wire JSON carries the new field name and not the old one.
	raw := rawToolResult(t, sess, "list_sessions", map[string]any{})
	if !strings.Contains(raw, `"last_active_at"`) {
		t.Errorf("list_sessions JSON missing last_active_at field: %s", raw)
	}
	if strings.Contains(raw, `"updated_at"`) {
		t.Errorf("list_sessions JSON still carries the retired updated_at field: %s", raw)
	}

	// The value is the session's ended_at (~60m ago), the same recency the feed shows.
	out := callJSON(t, sess, "list_sessions", map[string]any{})
	rows, _ := out["sessions"].([]any)
	if len(rows) != 1 {
		t.Fatalf("want 1 session, got %d: %+v", len(rows), out)
	}
	la, ok := rows[0].(map[string]any)["last_active_at"].(string)
	if !ok || la == "" {
		t.Fatalf("row missing last_active_at: %+v", rows[0])
	}
	ts, err := time.Parse(time.RFC3339, la)
	if err != nil {
		t.Fatalf("parse last_active_at %q: %v", la, err)
	}
	if d := time.Since(ts); d < 30*time.Minute || d > 3*time.Hour {
		t.Errorf("last_active_at = %v (%.0fm ago), want ~60m (the session's ended_at)", ts, d.Minutes())
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

// TestModelFallbacksSurfaced pins the model-fallback surface end to end: the feed row
// and the session header carry model_fallback_count, and get_session carries a
// model_fallbacks list only when the count is above zero and only on the first view (a
// header-only call or the first transcript window), mirroring DuplicateToolCallIDs. A fully
// merged row reports declined_tokens; a system-only row (no observed spend) omits that
// object and leaves the nullable fields null. A session without fallbacks omits
// model_fallbacks entirely.
func TestModelFallbacksSurfaced(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	fx := seedSession(t, st) // one session, no fallbacks
	ctx := context.Background()
	sess := connect(t, st)

	// Seed a second session with two fallbacks: one fully merged (message ordinal, all
	// four declined token classes, category and explanation) and one system-only (no
	// ordinal, no spend), so the DTO's declined_tokens omitempty is exercised both ways.
	var owner int64
	if err := st.Pool.QueryRow(ctx, `SELECT user_id FROM sessions WHERE id = $1`, fx.sessionID).Scan(&owner); err != nil {
		t.Fatalf("owner: %v", err)
	}
	var fbSession int64
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine,
		   message_count, user_message_count, model_fallback_count)
		 VALUES ($1,$2,'claude','src-fb','box',1,1,2) RETURNING id`,
		owner, fx.projectID).Scan(&fbSession); err != nil {
		t.Fatalf("fallback session: %v", err)
	}
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO model_fallbacks (session_id, message_ordinal, from_model, to_model, trigger,
		   refusal_category, refusal_explanation,
		   declined_input_tokens, declined_output_tokens, declined_cache_write_tokens, declined_cache_read_tokens,
		   occurred_at, dedup_key)
		 VALUES ($1, 3, 'claude-fable-5', 'claude-opus-4-8', 'fallback',
		         'safety', 'declined for policy', 11, 22, 33, 44, now(), 'req-merged'),
		        ($1, NULL, '', '', 'model_refusal_fallback',
		         NULL, NULL, NULL, NULL, NULL, NULL, NULL, 'req-system-only')`,
		fbSession); err != nil {
		t.Fatalf("model_fallbacks: %v", err)
	}

	// The seeded no-fallback session reports a zero count and omits the list.
	none := callJSON(t, sess, "get_session", map[string]any{"session_id": fx.sessionID})
	if c, ok := none["model_fallback_count"].(float64); !ok || c != 0 {
		t.Fatalf("no-fallback session model_fallback_count = %v, want 0: %+v", none["model_fallback_count"], none)
	}
	if _, present := none["model_fallbacks"]; present {
		t.Fatalf("no-fallback session must omit model_fallbacks: %+v", none)
	}

	// The fallback session's header carries the count and the ordered list.
	det := callJSON(t, sess, "get_session", map[string]any{"session_id": fbSession})
	if c, ok := det["model_fallback_count"].(float64); !ok || c != 2 {
		t.Fatalf("fallback session model_fallback_count = %v, want 2: %+v", det["model_fallback_count"], det)
	}
	fbs, _ := det["model_fallbacks"].([]any)
	if len(fbs) != 2 {
		t.Fatalf("want 2 model_fallbacks, got %d: %+v", len(fbs), det["model_fallbacks"])
	}
	// Order is by occurred_at then dedup_key; the merged row (with a timestamp) sorts
	// before the system-only row (a null timestamp sorts last).
	merged := fbs[0].(map[string]any)
	if merged["from_model"] != "claude-fable-5" || merged["to_model"] != "claude-opus-4-8" {
		t.Fatalf("merged fallback models wrong: %+v", merged)
	}
	if int(merged["message_ordinal"].(float64)) != 3 || merged["refusal_category"] != "safety" {
		t.Fatalf("merged fallback fields wrong: %+v", merged)
	}
	dt, ok := merged["declined_tokens"].(map[string]any)
	if !ok {
		t.Fatalf("merged fallback missing declined_tokens: %+v", merged)
	}
	if int(dt["input"].(float64)) != 11 || int(dt["output"].(float64)) != 22 ||
		int(dt["cache_write"].(float64)) != 33 || int(dt["cache_read"].(float64)) != 44 {
		t.Fatalf("declined_tokens wrong: %+v", dt)
	}

	// A header-only view (no transcript) still carries the list: it is a first view.
	header := callJSON(t, sess, "get_session", map[string]any{"session_id": fbSession, "include_transcript": false})
	if _, present := header["model_fallbacks"]; !present {
		t.Fatalf("header-only view should carry model_fallbacks: %+v", header)
	}
	// A later transcript page (transcript_after set) omits the list, the same gate
	// duplicate_tool_call_ids uses, so paging a long session does not re-read the rows.
	// The count still rides the header on every page.
	paged := callJSON(t, sess, "get_session", map[string]any{"session_id": fbSession, "transcript_after": 1})
	if _, present := paged["model_fallbacks"]; present {
		t.Fatalf("paged view should omit model_fallbacks: %+v", paged)
	}
	if c, ok := paged["model_fallback_count"].(float64); !ok || c != 2 {
		t.Fatalf("paged view should still carry model_fallback_count = 2: %+v", paged)
	}

	// The system-only row omits declined_tokens and leaves the nullable fields null.
	sysOnly := fbs[1].(map[string]any)
	if _, present := sysOnly["declined_tokens"]; present {
		t.Fatalf("system-only fallback must omit declined_tokens: %+v", sysOnly)
	}
	if mo, present := sysOnly["message_ordinal"]; !present || mo != nil {
		t.Fatalf("system-only fallback message_ordinal should be null: %+v", sysOnly)
	}
	if sysOnly["trigger"] != "model_refusal_fallback" {
		t.Fatalf("system-only fallback trigger wrong: %+v", sysOnly)
	}

	// The feed row carries the per-session count too.
	feed := callJSON(t, sess, "list_sessions", map[string]any{})
	rows, _ := feed["sessions"].([]any)
	var seen bool
	for _, r := range rows {
		row := r.(map[string]any)
		if int(row["id"].(float64)) == int(fbSession) {
			seen = true
			if c, ok := row["model_fallback_count"].(float64); !ok || c != 2 {
				t.Fatalf("feed row model_fallback_count = %v, want 2: %+v", row["model_fallback_count"], row)
			}
		}
	}
	if !seen {
		t.Fatalf("fallback session missing from feed: %+v", feed)
	}
}

// TestModelFallbackPartialDeclinedOmitsObject pins the DTO's all-or-nothing rule for the
// declined-tokens object: a row with exactly one nil declined class (only three of four
// merged in) omits declined_tokens entirely rather than reporting a partial, misleading zero.
func TestModelFallbackPartialDeclinedOmitsObject(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	fx := seedSession(t, st)
	ctx := context.Background()
	sess := connect(t, st)

	var owner int64
	if err := st.Pool.QueryRow(ctx, `SELECT user_id FROM sessions WHERE id = $1`, fx.sessionID).Scan(&owner); err != nil {
		t.Fatalf("owner: %v", err)
	}
	var partial int64
	if err := st.Pool.QueryRow(ctx,
		`INSERT INTO sessions (user_id, project_id, agent, source_session_id, machine,
		   message_count, user_message_count, model_fallback_count)
		 VALUES ($1,$2,'claude','src-partial','box',1,1,1) RETURNING id`,
		owner, fx.projectID).Scan(&partial); err != nil {
		t.Fatalf("partial session: %v", err)
	}
	// Three of four declined classes present, cache_read still NULL: the object must be omitted.
	if _, err := st.Pool.Exec(ctx,
		`INSERT INTO model_fallbacks (session_id, message_ordinal, from_model, to_model, trigger,
		   declined_input_tokens, declined_output_tokens, declined_cache_write_tokens, declined_cache_read_tokens,
		   occurred_at, dedup_key)
		 VALUES ($1, 1, 'claude-fable-5', 'claude-opus-4-8', 'refusal', 10, 20, 30, NULL, now(), 'req-partial')`,
		partial); err != nil {
		t.Fatalf("partial fallback: %v", err)
	}

	det := callJSON(t, sess, "get_session", map[string]any{"session_id": partial})
	fbs, _ := det["model_fallbacks"].([]any)
	if len(fbs) != 1 {
		t.Fatalf("want 1 model_fallback, got %d: %+v", len(fbs), det["model_fallbacks"])
	}
	row := fbs[0].(map[string]any)
	if _, present := row["declined_tokens"]; present {
		t.Errorf("a partly measured declined spend must omit declined_tokens, got %+v", row["declined_tokens"])
	}
}

// TestSubagentModelFallbackCountThroughParent pins the projection-consistency fix at the MCP
// surface: a child session with a fallback rollup reports its real model_fallback_count in the
// parent's get_session subagents list, not a phantom zero. The subagent DTO is built from the
// shared SessionSummary read, so the count must ride that read.
func TestSubagentModelFallbackCountThroughParent(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	fx := seedSession(t, st)
	ctx := context.Background()
	sess := connect(t, st)

	var owner int64
	var parentSrc string
	if err := st.Pool.QueryRow(ctx, `SELECT user_id, source_session_id FROM sessions WHERE id = $1`, fx.sessionID).Scan(&owner, &parentSrc); err != nil {
		t.Fatalf("owner: %v", err)
	}
	// A child whose source id nests under the parent's, linked on announce, with a fallback rollup.
	child, err := st.Announce(ctx, store.AnnounceParams{
		UserID: owner, Agent: "claude", SourceSessionID: parentSrc + "/subagents/agent-abc", ProjectID: fx.projectID,
	})
	if err != nil {
		t.Fatalf("announce child: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, "UPDATE sessions SET model_fallback_count = 5 WHERE id = $1", child.SessionID); err != nil {
		t.Fatalf("stamp child rollup: %v", err)
	}

	det := callJSON(t, sess, "get_session", map[string]any{"session_id": fx.sessionID})
	subs, _ := det["subagents"].([]any)
	if len(subs) != 1 {
		t.Fatalf("want 1 subagent, got %d: %+v", len(subs), det["subagents"])
	}
	sub := subs[0].(map[string]any)
	if c, ok := sub["model_fallback_count"].(float64); !ok || c != 5 {
		t.Errorf("subagent model_fallback_count = %v, want 5 (the summary read must carry the rollup)", sub["model_fallback_count"])
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
