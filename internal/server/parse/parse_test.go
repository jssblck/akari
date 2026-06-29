package parse

import (
	"context"
	"testing"

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
// Claude repeats the same usage block across lines (a sidechain or summary
// duplicate carries the same message id, hence the same dedup_key), so the usage
// ledger keeps exactly one row while a naive per-region fold added every
// occurrence (the 2.4x to 3.6x inflation seen in production). The invariant the
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
