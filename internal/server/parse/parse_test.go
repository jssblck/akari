package parse

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jssblck/akari/internal/parser"
	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/internal/server/storetest"
)

// The three lines of a minimal Claude session: one user turn, one assistant turn
// with a tool use and token usage, then a user turn carrying only the tool
// result. Kept as separate lines so a test can upload them across chunk
// boundaries.
var claudeLines = []string{
	`{"type":"user","timestamp":"2024-01-01T10:00:00Z","message":{"content":"Fix the bug"},"cwd":"/home/grace/akari","gitBranch":"main"}` + "\n",
	`{"type":"assistant","timestamp":"2024-01-01T10:00:05Z","message":{"id":"msg_1","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"On it."},{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"auth.go"}}],"usage":{"input_tokens":1000000,"output_tokens":1000000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}` + "\n",
	`{"type":"user","timestamp":"2024-01-01T10:00:06Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"package auth","is_error":false}]}}` + "\n",
}

// firstUser registers the bootstrap admin (the only registration a fresh schema
// allows without an invite) and returns its id.
func firstUser(t *testing.T, st *store.Store) int64 {
	t.Helper()
	u, err := st.Register(context.Background(), "grace", "hash", "")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return u.ID
}

// seedSession announces a fresh Claude session and returns its id.
func seedSession(t *testing.T, st *store.Store, source string) int64 {
	t.Helper()
	ctx := context.Background()
	uid := firstUser(t, st)
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: uid, Agent: "claude", SourceSessionID: source,
		ProjectID: projectID, GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	return ann.SessionID
}

// uploadAndParse appends each piece as its own chunk and advances the parse after
// each, returning the final message count.
func uploadAndParse(t *testing.T, st *store.Store, sessionID int64, pieces ...string) int {
	t.Helper()
	ctx := context.Background()
	var offset int64
	var msgCount int
	for _, p := range pieces {
		stored, err := st.AppendChunk(ctx, sessionID, offset, []byte(p))
		if err != nil {
			t.Fatalf("append at %d: %v", offset, err)
		}
		offset = stored
		if msgCount, err = Advance(ctx, st, sessionID, "claude"); err != nil {
			t.Fatalf("advance: %v", err)
		}
	}
	return msgCount
}

func TestAdvanceSingleChunk(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedSession(t, st, "single")

	whole := claudeLines[0] + claudeLines[1] + claudeLines[2]
	if mc := uploadAndParse(t, st, sid, whole); mc != 2 {
		t.Fatalf("message count = %d, want 2", mc)
	}
	assertClaudeProjection(t, st, sid)

	// Advancing again with nothing new is a no-op.
	if mc, err := Advance(ctx, st, sid, "claude"); err != nil || mc != 2 {
		t.Fatalf("re-advance: mc=%d err=%v", mc, err)
	}
	assertClaudeProjection(t, st, sid)

	// A full reparse rebuilds the same projection.
	if mc, err := Reparse(ctx, st, sid, "claude"); err != nil || mc != 2 {
		t.Fatalf("reparse: mc=%d err=%v", mc, err)
	}
	assertClaudeProjection(t, st, sid)
}

// TestAdvanceChunkedMatchesSingle uploads the same session line by line, so the
// tool result is back-patched in a later chunk than its call, and confirms the
// projection is identical to the single-shot upload.
func TestAdvanceChunkedMatchesSingle(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	sid := seedSession(t, st, "chunked")

	if mc := uploadAndParse(t, st, sid, claudeLines[0], claudeLines[1], claudeLines[2]); mc != 2 {
		t.Fatalf("message count = %d, want 2", mc)
	}
	assertClaudeProjection(t, st, sid)
}

// assertClaudeProjection checks the derived rows and aggregates a fully parsed
// claudeLines session must have, however it was chunked.
func assertClaudeProjection(t *testing.T, st *store.Store, sid int64) {
	t.Helper()
	ctx := context.Background()

	var messages, tools, usage int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM messages WHERE session_id=$1", sid).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 2 {
		t.Errorf("messages rows = %d, want 2", messages)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM tool_calls WHERE session_id=$1", sid).Scan(&tools); err != nil {
		t.Fatal(err)
	}
	if tools != 1 {
		t.Errorf("tool_calls rows = %d, want 1", tools)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM usage_events WHERE session_id=$1", sid).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if usage != 1 {
		t.Errorf("usage_events rows = %d, want 1", usage)
	}

	var status string
	var resultBytes int64
	if err := st.Pool.QueryRow(ctx,
		"SELECT result_status, result_bytes FROM tool_calls WHERE session_id=$1", sid).
		Scan(&status, &resultBytes); err != nil {
		t.Fatal(err)
	}
	if status != "ok" || resultBytes != int64(len("package auth")) {
		t.Errorf("tool result: status=%q bytes=%d", status, resultBytes)
	}

	var (
		mc, umc           int
		totalIn, totalOut int64
		cost              float64
		costIncomplete    bool
		parserVer         int
		startedAt, ended  *string
	)
	if err := st.Pool.QueryRow(ctx,
		`SELECT message_count, user_message_count, total_input_tokens, total_output_tokens,
		        total_cost_usd, cost_incomplete, parser_version, started_at::text, ended_at::text
		   FROM sessions WHERE id=$1`, sid).
		Scan(&mc, &umc, &totalIn, &totalOut, &cost, &costIncomplete, &parserVer, &startedAt, &ended); err != nil {
		t.Fatal(err)
	}
	if mc != 2 || umc != 1 {
		t.Errorf("counts: message=%d user=%d", mc, umc)
	}
	if totalIn != 1_000_000 || totalOut != 1_000_000 {
		t.Errorf("tokens: in=%d out=%d", totalIn, totalOut)
	}
	if cost < 17.999 || cost > 18.001 {
		t.Errorf("cost = %v, want ~18", cost)
	}
	if costIncomplete {
		t.Error("cost should be complete: sonnet is priced")
	}
	if parserVer != Version {
		t.Errorf("parser_version = %d, want %d", parserVer, Version)
	}
	if startedAt == nil || ended == nil {
		t.Error("started_at/ended_at should be set from message timestamps")
	}
}

// TestCodexTurnFoldedInOneChunk delivers a whole Codex turn (reasoning, tool
// call, and the assistant reply) in one chunk, the way the turn-aligned ingest
// protocol guarantees. The run of items folds into a single assistant message,
// closed by the following user turn, with its tool use recorded.
func TestCodexTurnFoldedInOneChunk(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uid := firstUser(t, st)
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: uid, Agent: "codex", SourceSessionID: "codex-fold", ProjectID: projectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	sid := ann.SessionID

	// One whole turn, then the user line that closes it, in a single chunk.
	chunk := `{"type":"session_meta","timestamp":"2024-01-01T10:00:00Z","payload":{"cwd":"/x","git":{"branch":"main"},"model":"gpt-5-codex"}}` + "\n" +
		`{"type":"response_item","timestamp":"2024-01-01T10:00:01Z","payload":{"type":"reasoning","content":[{"type":"text","text":"think A"}]}}` + "\n" +
		`{"type":"response_item","timestamp":"2024-01-01T10:00:02Z","payload":{"type":"function_call","name":"shell_command","arguments":"{}","call_id":"c1"}}` + "\n" +
		`{"type":"response_item","timestamp":"2024-01-01T10:00:03Z","payload":{"role":"assistant","content":[{"type":"output_text","text":"done"}]}}` + "\n" +
		`{"type":"response_item","timestamp":"2024-01-01T10:00:04Z","payload":{"role":"user","content":[{"type":"input_text","text":"next"}]}}` + "\n"

	if _, err := st.AppendChunk(ctx, sid, 0, []byte(chunk)); err != nil {
		t.Fatal(err)
	}
	if _, err := Advance(ctx, st, sid, "codex"); err != nil {
		t.Fatalf("advance: %v", err)
	}

	var content, thinking string
	var hasTool bool
	if err := st.Pool.QueryRow(ctx,
		"SELECT content, thinking_text, has_tool_use FROM messages WHERE session_id=$1 AND ordinal=0", sid).
		Scan(&content, &thinking, &hasTool); err != nil {
		t.Fatal(err)
	}
	if content != "done" || thinking != "think A" || !hasTool {
		t.Fatalf("folded turn: content=%q thinking=%q tool=%v", content, thinking, hasTool)
	}

	var mc int
	if err := st.Pool.QueryRow(ctx, "SELECT message_count FROM sessions WHERE id=$1", sid).Scan(&mc); err != nil {
		t.Fatal(err)
	}
	if mc != 2 {
		t.Fatalf("message_count = %d, want 2 (one assistant, one user)", mc)
	}
	var calls int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM tool_calls WHERE session_id=$1 AND message_ordinal=0", sid).Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("tool calls on the folded turn = %d, want 1", calls)
	}
}

// TestRedactedThinkingReachesMessagesColumn drives a Claude turn whose reasoning is
// redacted to empty text with only a signature (what the current client emits) through
// the full ingest and parse path, and confirms the reasoning volume survives to
// messages.thinking_bytes with has_thinking set. This is the end-to-end guard for the
// Epoch 11 -> 12 change: the original observed-thinking implementation keyed on
// thinking_text and read zero for every redacted turn, so this pins that the encrypted
// payload length reaches the column the observed-thinking signal sums.
func TestRedactedThinkingReachesMessagesColumn(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedSession(t, st, "redacted-thinking")

	sig := strings.Repeat("s", 500)
	turn := `{"type":"user","timestamp":"2024-01-01T10:00:00Z","message":{"content":"think hard"}}` + "\n" +
		`{"type":"assistant","timestamp":"2024-01-01T10:00:05Z","message":{"id":"m1","model":"claude-opus-4-8","content":[{"type":"thinking","thinking":"","signature":"` + sig + `"},{"type":"text","text":"Done."}]}}` + "\n"
	if mc := uploadAndParse(t, st, sid, turn); mc != 2 {
		t.Fatalf("message count = %d, want 2", mc)
	}

	var hasThinking bool
	var thinkingText string
	var thinkingBytes int
	if err := st.Pool.QueryRow(ctx,
		"SELECT has_thinking, thinking_text, thinking_bytes FROM messages WHERE session_id=$1 AND role='assistant'", sid).
		Scan(&hasThinking, &thinkingText, &thinkingBytes); err != nil {
		t.Fatal(err)
	}
	if !hasThinking {
		t.Error("a redacted thinking block must still set has_thinking")
	}
	if thinkingText != "" {
		t.Errorf("redacted thinking has no text, got %q", thinkingText)
	}
	if thinkingBytes != len(sig) {
		t.Errorf("thinking_bytes = %d, want %d (signature length reaches the column)", thinkingBytes, len(sig))
	}
}

// TestCodexContextExcludedFromCounts drives a Codex session's injected framing (the
// AGENTS.md-plus-environment turn Codex prepends before the real prompt) through the
// full ingest and parse path and confirms it is recorded as context, not a human
// prompt: it counts toward message_count but not user_message_count, and the session
// title reads the real opening prompt rather than the AGENTS.md block. This is the
// end-to-end guard for the parse.Epoch 7 -> 8 re-roling, exercising the aggregate fold
// (applyAggregates) that ApplyProjectionDelta alone does not run.
func TestCodexContextExcludedFromCounts(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uid := firstUser(t, st)
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: uid, Agent: "codex", SourceSessionID: "codex-context", ProjectID: projectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	sid := ann.SessionID

	// session_meta, the developer instructions (dropped), the AGENTS.md + environment_context turn
	// (context), the real prompt, and an assistant reply, all in one chunk.
	chunk := `{"type":"session_meta","timestamp":"2024-01-01T10:00:00Z","payload":{"cwd":"/home/ada/akari","git":{"branch":"main"},"model":"gpt-5-codex"}}` + "\n" +
		`{"type":"response_item","timestamp":"2024-01-01T10:00:00Z","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions>never</permissions instructions>"}]}}` + "\n" +
		`{"type":"response_item","timestamp":"2024-01-01T10:00:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /home/ada/akari\n\n<INSTRUCTIONS>\nRun make build.\n</INSTRUCTIONS>"},{"type":"input_text","text":"<environment_context>\n  <cwd>/home/ada/akari</cwd>\n</environment_context>"}]}}` + "\n" +
		`{"type":"response_item","timestamp":"2024-01-01T10:00:01Z","payload":{"role":"user","content":[{"type":"input_text","text":"Add rate limiting"}]}}` + "\n" +
		`{"type":"response_item","timestamp":"2024-01-01T10:00:05Z","payload":{"role":"assistant","content":[{"type":"output_text","text":"On it."}]}}` + "\n"

	if _, err := st.AppendChunk(ctx, sid, 0, []byte(chunk)); err != nil {
		t.Fatal(err)
	}
	if _, err := Advance(ctx, st, sid, "codex"); err != nil {
		t.Fatalf("advance: %v", err)
	}

	// The context turn, the prompt, and the assistant reply are all messages; only the prompt is a
	// human turn. The developer instructions are dropped.
	var mc, umc int
	if err := st.Pool.QueryRow(ctx,
		"SELECT message_count, user_message_count FROM sessions WHERE id=$1", sid).Scan(&mc, &umc); err != nil {
		t.Fatal(err)
	}
	if mc != 3 {
		t.Errorf("message_count = %d, want 3 (context, prompt, assistant)", mc)
	}
	if umc != 1 {
		t.Errorf("user_message_count = %d, want 1 (only the real prompt; the context turn must not count)", umc)
	}

	// The role of ordinal 0 is context, and the title reads the real prompt, not the AGENTS.md block.
	var role string
	if err := st.Pool.QueryRow(ctx,
		"SELECT role FROM messages WHERE session_id=$1 AND ordinal=0", sid).Scan(&role); err != nil {
		t.Fatal(err)
	}
	if role != "context" {
		t.Errorf("ordinal 0 role = %q, want context", role)
	}
	d, err := st.SessionDetailByID(ctx, sid)
	if err != nil {
		t.Fatalf("session detail: %v", err)
	}
	if d.Title != "Add rate limiting" {
		t.Errorf("title = %q, want the real opening prompt", d.Title)
	}
}

// TestCodexTrailingTurnFlushedWhole confirms the final turn of a session, which
// has no closing user line, still parses as one complete message: the protocol
// flushes it whole in the last chunk, and the reducer emits the open turn at the
// region's end rather than carrying it.
func TestCodexTrailingTurnFlushedWhole(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()

	uid := firstUser(t, st)
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: uid, Agent: "codex", SourceSessionID: "codex-trailing", ProjectID: projectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	sid := ann.SessionID

	// A user line, then the assistant's reply, with no following user line: the
	// session ended here.
	chunk := `{"type":"session_meta","timestamp":"2024-01-01T10:00:00Z","payload":{"cwd":"/x","model":"gpt-5-codex"}}` + "\n" +
		`{"type":"response_item","timestamp":"2024-01-01T10:00:01Z","payload":{"role":"user","content":[{"type":"input_text","text":"hi"}]}}` + "\n" +
		`{"type":"response_item","timestamp":"2024-01-01T10:00:02Z","payload":{"role":"assistant","content":[{"type":"output_text","text":"hello"}]}}` + "\n"

	if _, err := st.AppendChunk(ctx, sid, 0, []byte(chunk)); err != nil {
		t.Fatal(err)
	}
	if _, err := Advance(ctx, st, sid, "codex"); err != nil {
		t.Fatalf("advance: %v", err)
	}

	var content string
	if err := st.Pool.QueryRow(ctx,
		"SELECT content FROM messages WHERE session_id=$1 AND ordinal=1", sid).Scan(&content); err != nil {
		t.Fatal(err)
	}
	if content != "hello" {
		t.Fatalf("trailing assistant content = %q, want %q", content, "hello")
	}
}

// TestClaudeDuplicateCallUIDDoesNotAbort reproduces the reparse failure that kept
// four production sessions stale: a resumed or compacted Claude transcript replays
// a prior assistant turn verbatim, so two distinct tool_use rows carry the same
// agent call id. Under the old unique index the second insert tripped it and rolled
// the whole parse back. With the index non-unique (migration 0010) both rows keep
// the id, and the back-patch stamps the same result onto each, so every replayed
// copy of the turn renders with its result rather than one looking pending. The two
// turns are delivered as separate chunks so they parse in separate regions (and so
// separate transactions), the cross-region path the back-patch must cover.
func TestClaudeDuplicateCallUIDDoesNotAbort(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedSession(t, st, "claude-dup-calluid")

	// Two assistant turns whose tool_use blocks share id "toolu_dup" (the replay), and
	// a user turn whose tool_result names that id. The message ids differ so this
	// isolates the call_uid collision from the usage dedup path.
	first := `{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","message":{"id":"msg_a","model":"claude-sonnet-4-20250514","content":[{"type":"tool_use","id":"toolu_dup","name":"Read","input":{"file_path":"auth.go"}}]}}` + "\n"
	second := `{"type":"assistant","timestamp":"2024-01-01T10:00:02Z","message":{"id":"msg_b","model":"claude-sonnet-4-20250514","content":[{"type":"tool_use","id":"toolu_dup","name":"Read","input":{"file_path":"auth.go"}}]}}` + "\n"
	result := `{"type":"user","timestamp":"2024-01-01T10:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_dup","content":"package auth","is_error":false}]}}` + "\n"

	// Separate chunks: the first region commits before the second parses, so the
	// duplicate insert meets an already-committed row carrying the same id.
	uploadAndParse(t, st, sid, first, second, result)

	assertDuplicateCallUID := func(t *testing.T, when string) {
		t.Helper()
		var total, withUID, patched int
		if err := st.Pool.QueryRow(ctx,
			`SELECT count(*),
			        count(*) FILTER (WHERE call_uid = 'toolu_dup'),
			        count(*) FILTER (WHERE result_status = 'ok')
			   FROM tool_calls WHERE session_id=$1`, sid).
			Scan(&total, &withUID, &patched); err != nil {
			t.Fatal(err)
		}
		if total != 2 {
			t.Fatalf("%s: tool_calls rows = %d, want 2 (both turns kept)", when, total)
		}
		if withUID != 2 {
			t.Fatalf("%s: rows carrying the shared id = %d, want 2 (both keep it)", when, withUID)
		}
		// The back-patch keys on the id, so both copies of the replayed call carry the
		// same result rather than one of them looking pending.
		if patched != 2 {
			t.Fatalf("%s: rows with a back-patched result = %d, want 2", when, patched)
		}
		var bytes int64
		if err := st.Pool.QueryRow(ctx,
			`SELECT min(result_bytes) FROM tool_calls WHERE session_id=$1`, sid).Scan(&bytes); err != nil {
			t.Fatal(err)
		}
		if bytes != int64(len("package auth")) {
			t.Fatalf("%s: result_bytes = %d, want %d on every copy", when, bytes, len("package auth"))
		}
	}

	assertDuplicateCallUID(t, "after advance")

	// Reparse is the production remediation for the four stalled sessions: it must run
	// to completion and land the same shape rather than rolling back as it did before.
	if _, err := Reparse(ctx, st, sid, "claude"); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	assertDuplicateCallUID(t, "after reparse")
}

// TestClaudeModelFallbackMergesAndCounts ingests a full fallback sequence (two assistant
// chunks sharing a message id and requestId, one carrying the fallback block and both the
// iterations, then the system model_refusal_fallback entry sharing the requestId) and asserts
// the three parser ops merge to exactly one model_fallbacks row with fields from both sides,
// that sessions.model_fallback_count is 1, and that a reparse does not inflate either (still 1
// row, count still 1). It also drives the two read paths that surface the count and the
// SessionModelFallbacks ordered read.
func TestClaudeModelFallbackMergesAndCounts(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedSession(t, st, "claude-fallback")

	iters := `"iterations":[{"input_tokens":900,"output_tokens":6,"cache_read_input_tokens":3200,"cache_creation_input_tokens":1500,"type":"message","model":"claude-fable-5"},{"input_tokens":900,"output_tokens":260,"cache_read_input_tokens":5000,"cache_creation_input_tokens":0,"type":"fallback_message","model":"claude-opus-4-8"}]`
	// Whole sequence in one chunk: the block chunk, the text chunk (same id + requestId), and
	// the system entry, exactly the shapes the specimen file carries.
	chunk := `{"type":"assistant","timestamp":"2024-01-01T10:00:25Z","requestId":"req_fb","message":{"id":"msg_fb","model":"claude-opus-4-8","content":[{"type":"fallback","from":{"model":"claude-fable-5"},"to":{"model":"claude-opus-4-8"}}],"usage":{"input_tokens":900,"output_tokens":260,"cache_read_input_tokens":5000,` + iters + `}}}` + "\n" +
		`{"type":"assistant","timestamp":"2024-01-01T10:00:26Z","requestId":"req_fb","message":{"id":"msg_fb","model":"claude-opus-4-8","content":[{"type":"text","text":"honest working"}],"usage":{"input_tokens":900,"output_tokens":260,"cache_read_input_tokens":5000,` + iters + `}}}` + "\n" +
		`{"type":"system","subtype":"model_refusal_fallback","trigger":"refusal","originalModel":"claude-fable-5","fallbackModel":"claude-opus-4-8","requestId":"req_fb","apiRefusalCategory":"reasoning_extraction","apiRefusalExplanation":null,"timestamp":"2024-01-01T10:00:26Z"}` + "\n"

	if _, err := st.AppendChunk(ctx, sid, 0, []byte(chunk)); err != nil {
		t.Fatal(err)
	}
	if _, err := Advance(ctx, st, sid, "claude"); err != nil {
		t.Fatalf("advance: %v", err)
	}

	assertFallback := func(t *testing.T, when string) {
		t.Helper()
		var rows, count int
		if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM model_fallbacks WHERE session_id=$1", sid).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 1 {
			t.Fatalf("%s: model_fallbacks rows = %d, want 1 (three lines merge on the shared requestId)", when, rows)
		}
		if err := st.Pool.QueryRow(ctx, "SELECT model_fallback_count FROM sessions WHERE id=$1", sid).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s: model_fallback_count = %d, want 1", when, count)
		}
		// The one merged row carries fields from both sources: message_ordinal + declined
		// tokens from the assistant side, trigger + category from the system side.
		var ordinal, declIn, declCW *int
		var fromM, toM, trigger, category string
		if err := st.Pool.QueryRow(ctx,
			`SELECT message_ordinal, from_model, to_model, trigger, refusal_category,
			        declined_input_tokens, declined_cache_write_tokens
			   FROM model_fallbacks WHERE session_id=$1`, sid).
			Scan(&ordinal, &fromM, &toM, &trigger, &category, &declIn, &declCW); err != nil {
			t.Fatal(err)
		}
		if fromM != "claude-fable-5" || toM != "claude-opus-4-8" {
			t.Errorf("%s: models from=%q to=%q", when, fromM, toM)
		}
		if trigger != "refusal" || category != "reasoning_extraction" {
			t.Errorf("%s: system-side fields trigger=%q category=%q (system entry did not merge)", when, trigger, category)
		}
		if ordinal == nil || declIn == nil || *declIn != 900 || declCW == nil || *declCW != 1500 {
			t.Errorf("%s: assistant-side fields ordinal=%v declIn=%v declCW=%v (assistant side did not merge)", when, ordinal, declIn, declCW)
		}

		// The read path returns the one ordered row with the merged fields.
		fbs, err := st.SessionModelFallbacks(ctx, sid, store.ModelFallbackListCap)
		if err != nil {
			t.Fatal(err)
		}
		if len(fbs) != 1 {
			t.Fatalf("%s: SessionModelFallbacks = %d rows, want 1", when, len(fbs))
		}
		if fbs[0].Trigger != "refusal" || fbs[0].RefusalCategory != "reasoning_extraction" || fbs[0].MessageOrdinal == nil {
			t.Errorf("%s: read row = %+v", when, fbs[0])
		}

		// Both read paths that surface the count agree it is 1.
		detail, err := st.SessionDetailByID(ctx, sid)
		if err != nil {
			t.Fatal(err)
		}
		if detail.ModelFallbackCount != 1 {
			t.Errorf("%s: detail ModelFallbackCount = %d, want 1", when, detail.ModelFallbackCount)
		}
		feed, _, err := st.ListAllSessions(ctx, store.SessionFilter{})
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		for _, r := range feed {
			if r.ID == sid {
				found = true
				if r.ModelFallbackCount != 1 {
					t.Errorf("%s: feed row ModelFallbackCount = %d, want 1", when, r.ModelFallbackCount)
				}
			}
		}
		if !found {
			t.Errorf("%s: session not found in feed", when)
		}
	}

	assertFallback(t, "after advance")

	// A reparse must land the same one merged row and count, not double it.
	if _, err := Reparse(ctx, st, sid, "claude"); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	assertFallback(t, "after reparse")
}

// TestClaudeModelFallbackTurnIdentityMatchesUsage pins the reconciliation between the two
// projections a split fallback feeds: usage_events (via ON CONFLICT DO NOTHING, first line
// wins) and model_fallbacks (via ON CONFLICT DO UPDATE). The merged fallback row must keep
// the FIRST line's message_ordinal and occurred_at, the same turn identity usage_events
// pins, so the fallback notice never lands on a different turn than the usage it describes.
// The trap is a later line carrying a later timestamp: the first assistant chunk arrives at
// 10:00:25, and a separate later append brings the system model_refusal_fallback entry at
// 10:00:40. If the merge let a later non-null value overwrite, the fallback row would drift
// to 10:00:40 while the usage row stayed at 10:00:25. The test also pins the other half of
// the merge: the later system entry still fills trigger and category, the fill-toward-complete
// columns the assistant line lacks.
func TestClaudeModelFallbackTurnIdentityMatchesUsage(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedSession(t, st, "claude-fallback-turn-identity")

	iters := `"iterations":[{"input_tokens":900,"output_tokens":6,"cache_read_input_tokens":3200,"cache_creation_input_tokens":1500,"type":"message","model":"claude-fable-5"},{"input_tokens":900,"output_tokens":260,"cache_read_input_tokens":5000,"cache_creation_input_tokens":0,"type":"fallback_message","model":"claude-opus-4-8"}]`
	// First region: the two assistant chunks of one API message (shared id + requestId). The
	// block chunk at 10:00:25 inserts the fallback row and pins the usage row's turn identity;
	// the text chunk at 10:00:26 is the same message. This is the first (and only) assistant
	// turn, so it takes the earliest message ordinal.
	assistant := `{"type":"assistant","timestamp":"2024-01-01T10:00:25Z","requestId":"req_canon","message":{"id":"msg_canon","model":"claude-opus-4-8","content":[{"type":"fallback","from":{"model":"claude-fable-5"},"to":{"model":"claude-opus-4-8"}}],"usage":{"input_tokens":900,"output_tokens":260,"cache_read_input_tokens":5000,` + iters + `}}}` + "\n" +
		`{"type":"assistant","timestamp":"2024-01-01T10:00:26Z","requestId":"req_canon","message":{"id":"msg_canon","model":"claude-opus-4-8","content":[{"type":"text","text":"honest working"}],"usage":{"input_tokens":900,"output_tokens":260,"cache_read_input_tokens":5000,` + iters + `}}}` + "\n"
	if _, err := st.AppendChunk(ctx, sid, 0, []byte(assistant)); err != nil {
		t.Fatal(err)
	}
	if _, err := Advance(ctx, st, sid, "claude"); err != nil {
		t.Fatalf("advance assistant: %v", err)
	}

	// Second region: the system model_refusal_fallback entry, sharing the requestId but
	// carrying a strictly LATER timestamp (10:00:40) and a NULL ordinal. The merge must not
	// let either overwrite the pinned turn identity, even though it runs across region
	// boundaries (a separate append + advance).
	systemLine := `{"type":"system","subtype":"model_refusal_fallback","trigger":"refusal","originalModel":"claude-fable-5","fallbackModel":"claude-opus-4-8","requestId":"req_canon","apiRefusalCategory":"reasoning_extraction","apiRefusalExplanation":null,"timestamp":"2024-01-01T10:00:40Z"}` + "\n"
	if _, err := st.AppendChunk(ctx, sid, int64(len(assistant)), []byte(systemLine)); err != nil {
		t.Fatal(err)
	}
	if _, err := Advance(ctx, st, sid, "claude"); err != nil {
		t.Fatalf("advance system: %v", err)
	}

	assertTurnIdentity := func(t *testing.T, when string) {
		t.Helper()

		// The one merged fallback row: read its turn identity and the fill-toward-complete
		// columns the system entry carries.
		var fbOrdinal *int
		var fbOccurred time.Time
		var trigger, category string
		if err := st.Pool.QueryRow(ctx,
			`SELECT message_ordinal, occurred_at, trigger, refusal_category
			   FROM model_fallbacks WHERE session_id=$1`, sid).
			Scan(&fbOrdinal, &fbOccurred, &trigger, &category); err != nil {
			t.Fatal(err)
		}
		if fbOrdinal == nil {
			t.Fatalf("%s: fallback message_ordinal is NULL, want the assistant turn's ordinal", when)
		}
		// The fill-toward-complete half: the later system entry still filled trigger and
		// category, the fields the assistant line lacked.
		if trigger != "refusal" || category != "reasoning_extraction" {
			t.Errorf("%s: system entry did not fill descriptive columns: trigger=%q category=%q", when, trigger, category)
		}

		// The usage row for that same turn: found by the ordinal the assistant line took.
		// Its identity is pinned by usage_events' ON CONFLICT DO NOTHING to the first line.
		var usageOrdinal int
		var usageOccurred time.Time
		if err := st.Pool.QueryRow(ctx,
			`SELECT message_ordinal, occurred_at
			   FROM usage_events WHERE session_id=$1 AND message_ordinal=$2`, sid, *fbOrdinal).
			Scan(&usageOrdinal, &usageOccurred); err != nil {
			t.Fatalf("%s: no usage_events row at fallback ordinal %d: %v", when, *fbOrdinal, err)
		}

		// The invariant: both projections share one canonical turn identity. The fallback
		// row's ordinal and timestamp equal the usage row's, and the timestamp is the FIRST
		// assistant line's (10:00:25), never the later system entry's (10:00:40).
		if *fbOrdinal != usageOrdinal {
			t.Errorf("%s: ordinal drift: fallback=%d usage=%d", when, *fbOrdinal, usageOrdinal)
		}
		if !fbOccurred.Equal(usageOccurred) {
			t.Errorf("%s: occurred_at drift: fallback=%s usage=%s", when, fbOccurred, usageOccurred)
		}
		wantOccurred := time.Date(2024, 1, 1, 10, 0, 25, 0, time.UTC)
		if !fbOccurred.Equal(wantOccurred) {
			t.Errorf("%s: fallback occurred_at = %s, want the first assistant line's %s (a later line overwrote it)", when, fbOccurred.UTC(), wantOccurred)
		}
	}

	assertTurnIdentity(t, "after cross-region merge")

	// A reparse rebuilds both projections from scratch; the turn identity must still line up.
	if _, err := Reparse(ctx, st, sid, "claude"); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	assertTurnIdentity(t, "after reparse")
}

// TestClaudeModelFallbackSystemFirstTurnIdentityMatchesUsage is the system-first companion to
// TestClaudeModelFallbackTurnIdentityMatchesUsage. When the model_refusal_fallback system
// entry lands BEFORE the assistant entries, its NULL-ordinal row inserts first with its own
// (earlier here) timestamp as a placeholder. The later assistant line owns the turn: it fills
// message_ordinal, and the fallback row's occurred_at must adopt the assistant line's
// timestamp so it matches the usage_events row pinned at that ordinal, not stay on the system
// entry's placeholder. A first-non-null merge would freeze the system timestamp and drift the
// two projections apart, so this pins the CASE that rebinds occurred_at to the ordinal owner.
func TestClaudeModelFallbackSystemFirstTurnIdentityMatchesUsage(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedSession(t, st, "claude-fallback-system-first-identity")

	iters := `"iterations":[{"input_tokens":900,"output_tokens":6,"cache_read_input_tokens":3200,"cache_creation_input_tokens":1500,"type":"message","model":"claude-fable-5"},{"input_tokens":900,"output_tokens":260,"cache_read_input_tokens":5000,"cache_creation_input_tokens":0,"type":"fallback_message","model":"claude-opus-4-8"}]`
	// First region: only the system entry, at 10:00:20. Its row inserts with a NULL ordinal and
	// this timestamp as a placeholder, before any assistant line (or usage row) exists.
	systemLine := `{"type":"system","subtype":"model_refusal_fallback","trigger":"refusal","originalModel":"claude-fable-5","fallbackModel":"claude-opus-4-8","requestId":"req_sf_id","apiRefusalCategory":"reasoning_extraction","apiRefusalExplanation":null,"timestamp":"2024-01-01T10:00:20Z"}` + "\n"
	if _, err := st.AppendChunk(ctx, sid, 0, []byte(systemLine)); err != nil {
		t.Fatal(err)
	}
	if _, err := Advance(ctx, st, sid, "claude"); err != nil {
		t.Fatalf("advance system: %v", err)
	}

	// Second region: the assistant entries sharing the requestId, at 10:00:25 (LATER than the
	// system placeholder). This is the line usage_events pins to, so the fallback row must move
	// its occurred_at here even though the placeholder was earlier.
	assistant := `{"type":"assistant","timestamp":"2024-01-01T10:00:25Z","requestId":"req_sf_id","message":{"id":"msg_sf_id","model":"claude-opus-4-8","content":[{"type":"fallback","from":{"model":"claude-fable-5"},"to":{"model":"claude-opus-4-8"}}],"usage":{"input_tokens":900,"output_tokens":260,"cache_read_input_tokens":5000,` + iters + `}}}` + "\n" +
		`{"type":"assistant","timestamp":"2024-01-01T10:00:26Z","requestId":"req_sf_id","message":{"id":"msg_sf_id","model":"claude-opus-4-8","content":[{"type":"text","text":"honest working"}],"usage":{"input_tokens":900,"output_tokens":260,"cache_read_input_tokens":5000,` + iters + `}}}` + "\n"
	if _, err := st.AppendChunk(ctx, sid, int64(len(systemLine)), []byte(assistant)); err != nil {
		t.Fatal(err)
	}
	if _, err := Advance(ctx, st, sid, "claude"); err != nil {
		t.Fatalf("advance assistant: %v", err)
	}

	assertTurnIdentity := func(t *testing.T, when string) {
		t.Helper()
		var fbOrdinal *int
		var fbOccurred time.Time
		var trigger, category string
		if err := st.Pool.QueryRow(ctx,
			`SELECT message_ordinal, occurred_at, trigger, refusal_category
			   FROM model_fallbacks WHERE session_id=$1`, sid).
			Scan(&fbOrdinal, &fbOccurred, &trigger, &category); err != nil {
			t.Fatal(err)
		}
		if fbOrdinal == nil {
			t.Fatalf("%s: fallback message_ordinal is NULL after the assistant line arrived", when)
		}
		// The system entry inserted first, so its descriptive columns must survive the assistant
		// merge (fill-toward-complete does not clear a filled value).
		if trigger != "refusal" || category != "reasoning_extraction" {
			t.Errorf("%s: system-side fields lost across merge: trigger=%q category=%q", when, trigger, category)
		}

		var usageOccurred time.Time
		if err := st.Pool.QueryRow(ctx,
			`SELECT occurred_at FROM usage_events WHERE session_id=$1 AND message_ordinal=$2`, sid, *fbOrdinal).
			Scan(&usageOccurred); err != nil {
			t.Fatalf("%s: no usage_events row at fallback ordinal %d: %v", when, *fbOrdinal, err)
		}

		// The invariant: the fallback row rebound its timestamp to the assistant line that owns
		// the ordinal (10:00:25), matching usage_events, not the earlier system placeholder
		// (10:00:20).
		if !fbOccurred.Equal(usageOccurred) {
			t.Errorf("%s: occurred_at drift: fallback=%s usage=%s", when, fbOccurred, usageOccurred)
		}
		wantOccurred := time.Date(2024, 1, 1, 10, 0, 25, 0, time.UTC)
		if !fbOccurred.Equal(wantOccurred) {
			t.Errorf("%s: fallback occurred_at = %s, want the assistant line's %s (stuck on the system placeholder)", when, fbOccurred.UTC(), wantOccurred)
		}
	}

	assertTurnIdentity(t, "after system-first merge")

	if _, err := Reparse(ctx, st, sid, "claude"); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	assertTurnIdentity(t, "after reparse")
}

// TestClaudeModelSwitchIsNotAFallback is the negative control at the store level: two
// assistant turns with different model strings and no explicit markers (an intentional
// switch) produce zero model_fallbacks rows and leave model_fallback_count at 0.
func TestClaudeModelSwitchIsNotAFallback(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedSession(t, st, "claude-model-switch")

	chunk := `{"type":"assistant","timestamp":"2024-01-01T10:00:00Z","requestId":"req_1","message":{"id":"m1","model":"claude-fable-5","content":[{"type":"text","text":"hello"}],"usage":{"input_tokens":5,"output_tokens":5,"iterations":[{"input_tokens":5,"output_tokens":5,"type":"message"}]}}}` + "\n" +
		`{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","requestId":"req_2","message":{"id":"m2","model":"claude-opus-4-8","content":[{"type":"text","text":"now on opus"}],"usage":{"input_tokens":5,"output_tokens":5}}}` + "\n"

	if _, err := st.AppendChunk(ctx, sid, 0, []byte(chunk)); err != nil {
		t.Fatal(err)
	}
	if _, err := Advance(ctx, st, sid, "claude"); err != nil {
		t.Fatalf("advance: %v", err)
	}

	var rows, count int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM model_fallbacks WHERE session_id=$1", sid).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("model switch produced %d fallback rows, want 0", rows)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT model_fallback_count FROM sessions WHERE id=$1", sid).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("model_fallback_count = %d, want 0", count)
	}
}

// TestClaudeModelFallbackSystemFirstMerge pins the merge when the system entry lands before
// the assistant entries (the reverse of TestClaudeModelFallbackMergesAndCounts). The system
// row inserts first with only its refusal fields, the later assistant entries fill the
// message ordinal and declined tokens into the same (session_id, dedup_key) row, and
// model_fallback_count stays 1: the count folds once on the first insert of the dedup_key, so
// a later merge into the same key must not re-count.
func TestClaudeModelFallbackSystemFirstMerge(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedSession(t, st, "claude-fallback-system-first")

	iters := `"iterations":[{"input_tokens":900,"output_tokens":6,"cache_read_input_tokens":3200,"cache_creation_input_tokens":1500,"type":"message","model":"claude-fable-5"},{"input_tokens":900,"output_tokens":260,"cache_read_input_tokens":5000,"cache_creation_input_tokens":0,"type":"fallback_message","model":"claude-opus-4-8"}]`
	// First chunk: only the system entry, so its row inserts before the assistant side exists.
	systemLine := `{"type":"system","subtype":"model_refusal_fallback","trigger":"refusal","originalModel":"claude-fable-5","fallbackModel":"claude-opus-4-8","requestId":"req_sf","apiRefusalCategory":"reasoning_extraction","apiRefusalExplanation":null,"timestamp":"2024-01-01T10:00:20Z"}` + "\n"
	if _, err := st.AppendChunk(ctx, sid, 0, []byte(systemLine)); err != nil {
		t.Fatal(err)
	}
	if _, err := Advance(ctx, st, sid, "claude"); err != nil {
		t.Fatalf("advance system: %v", err)
	}

	// After the system-only advance the row exists with the refusal fields but no ordinal.
	var rows, count int
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM model_fallbacks WHERE session_id=$1", sid).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT model_fallback_count FROM sessions WHERE id=$1", sid).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || count != 1 {
		t.Fatalf("after system-only: rows=%d count=%d, want 1 and 1", rows, count)
	}

	// Second chunk: the two assistant entries sharing the requestId fill in the ordinal and
	// declined tokens on the same row.
	assistant := `{"type":"assistant","timestamp":"2024-01-01T10:00:25Z","requestId":"req_sf","message":{"id":"msg_sf","model":"claude-opus-4-8","content":[{"type":"fallback","from":{"model":"claude-fable-5"},"to":{"model":"claude-opus-4-8"}}],"usage":{"input_tokens":900,"output_tokens":260,"cache_read_input_tokens":5000,` + iters + `}}}` + "\n" +
		`{"type":"assistant","timestamp":"2024-01-01T10:00:26Z","requestId":"req_sf","message":{"id":"msg_sf","model":"claude-opus-4-8","content":[{"type":"text","text":"honest working"}],"usage":{"input_tokens":900,"output_tokens":260,"cache_read_input_tokens":5000,` + iters + `}}}` + "\n"
	if _, err := st.AppendChunk(ctx, sid, int64(len(systemLine)), []byte(assistant)); err != nil {
		t.Fatal(err)
	}
	if _, err := Advance(ctx, st, sid, "claude"); err != nil {
		t.Fatalf("advance assistant: %v", err)
	}

	// Still one row, still count 1, now carrying both sides' fields.
	var ordinal, declIn *int
	var fromM, toM, trigger, category string
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM model_fallbacks WHERE session_id=$1", sid).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT model_fallback_count FROM sessions WHERE id=$1", sid).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if rows != 1 || count != 1 {
		t.Fatalf("after assistant merge: rows=%d count=%d, want 1 and 1 (a merge must not re-count)", rows, count)
	}
	if err := st.Pool.QueryRow(ctx,
		`SELECT message_ordinal, from_model, to_model, trigger, refusal_category, declined_input_tokens
		   FROM model_fallbacks WHERE session_id=$1`, sid).
		Scan(&ordinal, &fromM, &toM, &trigger, &category, &declIn); err != nil {
		t.Fatal(err)
	}
	if trigger != "refusal" || category != "reasoning_extraction" {
		t.Errorf("system-side fields lost: trigger=%q category=%q", trigger, category)
	}
	if ordinal == nil || declIn == nil || *declIn != 900 {
		t.Errorf("assistant-side fields did not merge: ordinal=%v declIn=%v", ordinal, declIn)
	}
}

// TestCostIncompleteForUnknownModel confirms an unpriced model flips the
// session's cost_incomplete flag while still recording token totals.
func TestCostIncompleteForUnknownModel(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedSession(t, st, "unpriced")

	raw := `{"type":"assistant","message":{"id":"m1","model":"future-model-9","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":500,"output_tokens":500}}}` + "\n"
	if _, err := st.AppendChunk(ctx, sid, 0, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := Advance(ctx, st, sid, "claude"); err != nil {
		t.Fatal(err)
	}

	var cost float64
	var incomplete bool
	var totalIn int64
	if err := st.Pool.QueryRow(ctx,
		"SELECT total_cost_usd, cost_incomplete, total_input_tokens FROM sessions WHERE id=$1", sid).
		Scan(&cost, &incomplete, &totalIn); err != nil {
		t.Fatal(err)
	}
	if !incomplete {
		t.Error("unknown model should set cost_incomplete")
	}
	if cost != 0 {
		t.Errorf("cost = %v, want 0 for an unpriced model", cost)
	}
	if totalIn != 500 {
		t.Errorf("total_input_tokens = %d, want 500", totalIn)
	}
}

// TestClaudeDuplicateUsageCountedOnce reproduces the Claude rollup over-count.
// Claude streams one assistant message across several lines that share its message
// id, hence the same dedup_key, so the usage ledger keeps exactly one row while a
// naive per-region fold added every occurrence (the 2.4x to 3.6x inflation seen in
// production). The invariant the
// fix lands: the session rollups equal the deduped ledger, so total_* matches
// sum(usage_events.*) rather than a multiple of it, and message_count matches the
// count of messages rows.
func TestClaudeDuplicateUsageCountedOnce(t *testing.T) {
	t.Parallel()
	st := storetest.NewStore(t)
	ctx := context.Background()
	sid := seedSession(t, st, "claude-dup-usage")

	// The same assistant turn replayed three times, all sharing message id
	// "msg_dup" and the identical usage block. The dedup_key (message id) collides,
	// so usage_events keeps one row however many times the line appears.
	line := `{"type":"assistant","timestamp":"2024-01-01T10:00:05Z","message":{"id":"msg_dup","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":1000,"output_tokens":2000,"cache_creation_input_tokens":300,"cache_read_input_tokens":400}}}` + "\n"
	if _, err := st.AppendChunk(ctx, sid, 0, []byte(line+line+line)); err != nil {
		t.Fatal(err)
	}

	// assertRollupsMatchLedger is the invariant: for this session the rollups equal
	// the deduped ledger (total_* == sum(usage_events.*), message_count == count of
	// messages rows), and the single surviving usage row carries the real numbers.
	assertRollupsMatchLedger := func(t *testing.T, when string) {
		t.Helper()
		var usageRows int
		var ledgerIn, ledgerOut, ledgerCW, ledgerCR int64
		var ledgerCost float64
		if err := st.Pool.QueryRow(ctx,
			`SELECT count(*), coalesce(sum(input_tokens),0), coalesce(sum(output_tokens),0),
			        coalesce(sum(cache_write_tokens),0), coalesce(sum(cache_read_tokens),0),
			        coalesce(sum(cost_usd),0)
			   FROM usage_events WHERE session_id=$1`, sid).
			Scan(&usageRows, &ledgerIn, &ledgerOut, &ledgerCW, &ledgerCR, &ledgerCost); err != nil {
			t.Fatal(err)
		}
		if usageRows != 1 {
			t.Fatalf("%s: usage_events rows = %d, want 1 (deduped on dedup_key)", when, usageRows)
		}

		var rollIn, rollOut, rollCW, rollCR int64
		var rollCost float64
		var msgCount, rowCount int
		if err := st.Pool.QueryRow(ctx,
			`SELECT total_input_tokens, total_output_tokens, total_cache_write_tokens,
			        total_cache_read_tokens, total_cost_usd, message_count
			   FROM sessions WHERE id=$1`, sid).
			Scan(&rollIn, &rollOut, &rollCW, &rollCR, &rollCost, &msgCount); err != nil {
			t.Fatal(err)
		}
		if rollIn != ledgerIn || rollOut != ledgerOut || rollCW != ledgerCW || rollCR != ledgerCR {
			t.Fatalf("%s: rollup tokens (in=%d out=%d cw=%d cr=%d) != ledger (in=%d out=%d cw=%d cr=%d)",
				when, rollIn, rollOut, rollCW, rollCR, ledgerIn, ledgerOut, ledgerCW, ledgerCR)
		}
		if rollOut != 2000 {
			t.Fatalf("%s: total_output_tokens = %d, want 2000 (the single deduped row, not 6000)", when, rollOut)
		}
		if rollCost != ledgerCost {
			t.Fatalf("%s: total_cost_usd = %v != ledger cost %v", when, rollCost, ledgerCost)
		}
		if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM messages WHERE session_id=$1", sid).Scan(&rowCount); err != nil {
			t.Fatal(err)
		}
		if msgCount != rowCount {
			t.Fatalf("%s: message_count = %d, want %d (count of messages rows)", when, msgCount, rowCount)
		}
	}

	// The live incremental path folds the deduped set.
	if _, err := Advance(ctx, st, sid, "claude"); err != nil {
		t.Fatalf("advance: %v", err)
	}
	assertRollupsMatchLedger(t, "after advance")

	// Reparse is the remediation for already-ingested data: it zeroes the rollups
	// and replays the stored raw through the same fixed fold, so it must land the
	// same deduped totals rather than re-inflating them.
	if _, err := Reparse(ctx, st, sid, "claude"); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	assertRollupsMatchLedger(t, "after reparse")
}

// TestFallbackDeclinedProjectionGating pins toProjectionDelta's nullable mapping: an op that
// observed its declined spend (DeclinedObserved) carries the four counts as non-nil pointers
// so the store records measured values, while an op that never observed the spend (a fallback
// block with no iterations, or the system-side op) leaves all four nil so the store column
// stays NULL rather than reading a spurious zero-token attempt as measured.
func TestFallbackDeclinedProjectionGating(t *testing.T) {
	t.Parallel()
	ord := 1
	in := parser.Delta{Fallbacks: []parser.FallbackOp{
		{
			// Observed: the reducer summed the iteration entries.
			MessageOrdinal: &ord, FromModel: "claude-fable-5", ToModel: "claude-opus-4-8",
			DeclinedInput: 10, DeclinedOutput: 20, DeclinedCacheWrite: 30, DeclinedCacheRead: 40,
			DeclinedObserved: true, DedupKey: "observed",
		},
		{
			// Not observed: a fallback block rode this op but there were no iterations, so the
			// zero counts are unmeasured even though the op carries a message ordinal.
			MessageOrdinal: &ord, FromModel: "claude-fable-5", ToModel: "claude-opus-4-8",
			DeclinedObserved: false, DedupKey: "block-only",
		},
	}}
	d := toProjectionDelta(in)
	if len(d.Fallbacks) != 2 {
		t.Fatalf("projection fallbacks = %d, want 2", len(d.Fallbacks))
	}

	obs := d.Fallbacks[0]
	if obs.DeclinedInput == nil || *obs.DeclinedInput != 10 ||
		obs.DeclinedOutput == nil || *obs.DeclinedOutput != 20 ||
		obs.DeclinedCacheWrite == nil || *obs.DeclinedCacheWrite != 30 ||
		obs.DeclinedCacheRead == nil || *obs.DeclinedCacheRead != 40 {
		t.Errorf("observed op should carry non-nil measured counts, got %+v", obs)
	}

	unobs := d.Fallbacks[1]
	if unobs.DeclinedInput != nil || unobs.DeclinedOutput != nil ||
		unobs.DeclinedCacheWrite != nil || unobs.DeclinedCacheRead != nil {
		t.Errorf("unobserved op should leave all four declined pointers nil, got %+v", unobs)
	}
	// The op is still a fallback: only the declined spend is unmeasured, not the whole row.
	if unobs.MessageOrdinal == nil || unobs.FromModel != "claude-fable-5" {
		t.Errorf("unobserved op should still carry its non-token fields, got %+v", unobs)
	}
}
