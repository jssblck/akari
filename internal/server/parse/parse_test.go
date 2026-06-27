package parse

import (
	"context"
	"os"
	"testing"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/jssblck/akari/migrations"
)

// newTestStore mirrors the store package's harness: it connects to
// AKARI_TEST_DATABASE_URL, resets the schema, and applies migrations. Tests are
// skipped when the env var is unset.
func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("AKARI_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("set AKARI_TEST_DATABASE_URL to run parse integration tests")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, q := range []string{"DROP SCHEMA public CASCADE", "CREATE SCHEMA public"} {
		if _, err := st.Pool.Exec(ctx, q); err != nil {
			t.Fatalf("reset schema (%s): %v", q, err)
		}
	}
	if err := st.Migrate(ctx, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// claudeRaw is a minimal Claude session: one user turn, one assistant turn with
// a tool use and token usage, then a user turn carrying only the tool result.
const claudeRaw = `{"type":"user","timestamp":"2024-01-01T10:00:00Z","message":{"content":"Fix the bug"},"cwd":"/home/grace/akari","gitBranch":"main"}
{"type":"assistant","timestamp":"2024-01-01T10:00:05Z","message":{"id":"msg_1","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"On it."},{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"auth.go"}}],"usage":{"input_tokens":1000000,"output_tokens":1000000,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
{"type":"user","timestamp":"2024-01-01T10:00:06Z","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"package auth","is_error":false}]}}
`

func TestSessionFromRaw(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-1",
		ProjectID: projectID, GitBranch: "main", Cwd: "/home/grace/akari", Machine: "laptop",
	})
	if err != nil {
		t.Fatalf("announce: %v", err)
	}
	if _, err := st.AppendChunk(ctx, ann.SessionID, 0, []byte(claudeRaw)); err != nil {
		t.Fatalf("append: %v", err)
	}

	msgCount, err := SessionFromRaw(ctx, st, ann.SessionID, "claude")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if msgCount != 2 {
		t.Fatalf("message count = %d, want 2", msgCount)
	}

	// Derived rows landed.
	var messages, tools, usage int
	row := st.Pool.QueryRow(ctx, "SELECT count(*) FROM messages WHERE session_id=$1", ann.SessionID)
	if err := row.Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 2 {
		t.Errorf("messages rows = %d, want 2", messages)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM tool_calls WHERE session_id=$1", ann.SessionID).Scan(&tools); err != nil {
		t.Fatal(err)
	}
	if tools != 1 {
		t.Errorf("tool_calls rows = %d, want 1", tools)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM usage_events WHERE session_id=$1", ann.SessionID).Scan(&usage); err != nil {
		t.Fatal(err)
	}
	if usage != 1 {
		t.Errorf("usage_events rows = %d, want 1", usage)
	}

	// Tool result was applied and the body is stored only by size for now.
	var status string
	var resultBytes int64
	if err := st.Pool.QueryRow(ctx,
		"SELECT result_status, result_bytes FROM tool_calls WHERE session_id=$1", ann.SessionID).
		Scan(&status, &resultBytes); err != nil {
		t.Fatal(err)
	}
	if status != "ok" || resultBytes != int64(len("package auth")) {
		t.Errorf("tool result: status=%q bytes=%d", status, resultBytes)
	}

	// Session aggregates: 1M input + 1M output on sonnet-4 is exactly 18 USD.
	var (
		mc, umc            int
		totalIn, totalOut  int64
		cost               float64
		costIncomplete     bool
		parserVer          int
		startedAt, endedAt *string
	)
	if err := st.Pool.QueryRow(ctx,
		`SELECT message_count, user_message_count, total_input_tokens, total_output_tokens,
		        total_cost_usd, cost_incomplete, parser_version,
		        started_at::text, ended_at::text
		   FROM sessions WHERE id=$1`, ann.SessionID).
		Scan(&mc, &umc, &totalIn, &totalOut, &cost, &costIncomplete, &parserVer, &startedAt, &endedAt); err != nil {
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
	if startedAt == nil || endedAt == nil {
		t.Error("started_at/ended_at should be set from message timestamps")
	}

	// Reparsing is idempotent: counts do not double.
	if _, err := SessionFromRaw(ctx, st, ann.SessionID, "claude"); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if err := st.Pool.QueryRow(ctx, "SELECT count(*) FROM messages WHERE session_id=$1", ann.SessionID).Scan(&messages); err != nil {
		t.Fatal(err)
	}
	if messages != 2 {
		t.Errorf("after reparse messages rows = %d, want 2", messages)
	}
}

// TestCostIncompleteForUnknownModel confirms an unpriced model flips the
// session's cost_incomplete flag while still recording token totals.
func TestCostIncompleteForUnknownModel(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari")
	if err != nil {
		t.Fatal(err)
	}
	ann, err := st.Announce(ctx, store.AnnounceParams{
		UserID: u.ID, Agent: "claude", SourceSessionID: "sess-2", ProjectID: projectID,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw := `{"type":"assistant","message":{"id":"m1","model":"future-model-9","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":500,"output_tokens":500}}}` + "\n"
	if _, err := st.AppendChunk(ctx, ann.SessionID, 0, []byte(raw)); err != nil {
		t.Fatal(err)
	}
	if _, err := SessionFromRaw(ctx, st, ann.SessionID, "claude"); err != nil {
		t.Fatal(err)
	}

	var cost float64
	var incomplete bool
	var totalIn int64
	if err := st.Pool.QueryRow(ctx,
		"SELECT total_cost_usd, cost_incomplete, total_input_tokens FROM sessions WHERE id=$1", ann.SessionID).
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
