package parser

import (
	"os"
	"path/filepath"
	"testing"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

func TestParseClaude(t *testing.T) {
	s, err := Parse(AgentClaude, loadFixture(t, "claude.jsonl"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if s.Cwd != "/home/grace/code/my-app" {
		t.Errorf("cwd = %q", s.Cwd)
	}
	if s.GitBranch != "main" {
		t.Errorf("git branch = %q", s.GitBranch)
	}

	// The tool-result-only user turn is not a message.
	if len(s.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(s.Messages))
	}
	if s.Messages[0].Role != RoleUser || s.Messages[0].Content != "Fix the login bug" {
		t.Errorf("message 0 = %+v", s.Messages[0])
	}
	a := s.Messages[1]
	if a.Role != RoleAssistant || a.Content != "Looking at the auth module" {
		t.Errorf("message 1 content = %q", a.Content)
	}
	if !a.HasThinking || a.ThinkingText != "Consider the auth module" {
		t.Errorf("message 1 thinking = %q (has=%v)", a.ThinkingText, a.HasThinking)
	}
	if !a.HasToolUse {
		t.Errorf("message 1 should have tool use")
	}
	if a.Model != "claude-sonnet-4-20250514" {
		t.Errorf("message 1 model = %q", a.Model)
	}
	if s.Messages[2].Content != "Applied the fix." {
		t.Errorf("message 2 content = %q", s.Messages[2].Content)
	}

	if len(s.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(s.ToolCalls))
	}
	tc := s.ToolCalls[0]
	if tc.ToolName != "Read" || tc.Category != "read" || tc.FilePath != "src/auth.ts" {
		t.Errorf("tool call = %+v", tc)
	}
	if tc.ResultBody != "export function login() {}" || tc.ResultStatus != "ok" {
		t.Errorf("tool result = %q (%s)", tc.ResultBody, tc.ResultStatus)
	}
	if tc.ResultBytes != len("export function login() {}") || tc.ResultMediaType != "text/plain" {
		t.Errorf("tool result size = %d (%s)", tc.ResultBytes, tc.ResultMediaType)
	}
	if tc.MessageOrdinal != 1 {
		t.Errorf("tool call ordinal = %d, want 1", tc.MessageOrdinal)
	}

	if len(s.UsageEvent) != 2 {
		t.Fatalf("usage events = %d, want 2", len(s.UsageEvent))
	}
	u0 := s.UsageEvent[0]
	if u0.Input != 100 || u0.Output != 50 || u0.CacheWrite != 200 || u0.CacheRead != 300 {
		t.Errorf("usage 0 = %+v", u0)
	}
	if u0.MessageOrdinal == nil || *u0.MessageOrdinal != 1 {
		t.Errorf("usage 0 ordinal = %v", u0.MessageOrdinal)
	}
	if u0.DedupKey != "msg_1" {
		t.Errorf("usage 0 dedup = %q", u0.DedupKey)
	}
}

// TestParseClaudeSidechain covers the isSidechain flag: a Claude Code subagent's turn
// is written into the parent transcript flagged this way, and its usage must carry the
// flag so context-health analysis can read the main thread alone. A main-thread turn
// (no flag) reads false, and a flagged subagent turn reads true.
func TestParseClaudeSidechain(t *testing.T) {
	raw := []byte(`{"type":"user","timestamp":"2024-01-01T10:00:00Z","message":{"content":"do the thing"}}
{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","message":{"id":"main","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"on it"}],"usage":{"input_tokens":1000,"cache_read_input_tokens":5000}}}
{"type":"assistant","isSidechain":true,"timestamp":"2024-01-01T10:00:02Z","message":{"id":"sub","model":"claude-sonnet-4-20250514","content":[{"type":"text","text":"subagent working"}],"usage":{"input_tokens":40,"cache_read_input_tokens":60}}}
`)
	s, err := Parse(AgentClaude, raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(s.UsageEvent) != 2 {
		t.Fatalf("usage events = %d, want 2", len(s.UsageEvent))
	}
	if s.UsageEvent[0].IsSidechain {
		t.Errorf("main-thread usage should not be a sidechain: %+v", s.UsageEvent[0])
	}
	if !s.UsageEvent[1].IsSidechain {
		t.Errorf("subagent usage should be flagged as a sidechain: %+v", s.UsageEvent[1])
	}
}

// TestParseClaudeToolError covers an error tool result delivered as an array of
// text blocks: the status is "error", the body flattens to readable text, and the
// size and media type describe the flattened body.
func TestParseClaudeToolError(t *testing.T) {
	raw := []byte(`{"type":"user","timestamp":"2024-01-01T10:00:00Z","message":{"content":"run it"}}
{"type":"assistant","timestamp":"2024-01-01T10:00:01Z","message":{"id":"m1","model":"claude-sonnet-4-20250514","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"false"}}]}}
{"type":"user","timestamp":"2024-01-01T10:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"t1","is_error":true,"content":[{"type":"text","text":"command failed: exit 1"}]}]}}
`)
	s, err := Parse(AgentClaude, raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(s.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(s.ToolCalls))
	}
	tc := s.ToolCalls[0]
	if tc.ToolName != "Bash" {
		t.Errorf("tool name = %q", tc.ToolName)
	}
	if tc.ResultStatus != "error" {
		t.Errorf("result status = %q, want error", tc.ResultStatus)
	}
	if tc.ResultBody != "command failed: exit 1" {
		t.Errorf("result body = %q", tc.ResultBody)
	}
	if tc.ResultMediaType != "text/plain" || tc.ResultBytes != len("command failed: exit 1") {
		t.Errorf("result size = %d (%s)", tc.ResultBytes, tc.ResultMediaType)
	}
	// The input is captured as raw JSON for the CAS.
	if tc.InputJSON == "" || tc.FilePath != "" {
		t.Errorf("input = %q file=%q", tc.InputJSON, tc.FilePath)
	}
}

func TestParseCodex(t *testing.T) {
	s, err := Parse(AgentCodex, loadFixture(t, "codex.jsonl"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if s.Cwd != "/home/grace/code/my-api" {
		t.Errorf("cwd = %q", s.Cwd)
	}
	if s.GitBranch != "dev" {
		t.Errorf("git branch = %q", s.GitBranch)
	}

	// Two turns. The reasoning and function_call items precede the assistant text
	// and must fold into one assistant message per turn, not spawn extra turns.
	if len(s.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(s.Messages))
	}
	if s.Messages[0].Role != RoleUser || s.Messages[0].Content != "Add rate limiting" {
		t.Errorf("message 0 = %+v", s.Messages[0])
	}
	a := s.Messages[1]
	if a.Role != RoleAssistant || a.Content != "I'll add rate limiting." {
		t.Errorf("message 1 content = %q", a.Content)
	}
	if a.Model != "gpt-5-codex" {
		t.Errorf("message 1 model = %q", a.Model)
	}
	if !a.HasThinking || a.ThinkingText != "Choose a token bucket." {
		t.Errorf("message 1 thinking = %q (has=%v)", a.ThinkingText, a.HasThinking)
	}
	if !a.HasToolUse {
		t.Errorf("message 1 should have tool use")
	}
	if s.Messages[2].Role != RoleUser || s.Messages[2].Content != "Now add tests" {
		t.Errorf("message 2 = %+v", s.Messages[2])
	}
	a2 := s.Messages[3]
	if a2.Role != RoleAssistant || a2.Content != "Added tests." {
		t.Errorf("message 3 content = %q", a2.Content)
	}
	if a2.HasToolUse || a2.HasThinking {
		t.Errorf("message 3 should not carry the first turn's tool use or thinking: %+v", a2)
	}

	if len(s.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(s.ToolCalls))
	}
	tc := s.ToolCalls[0]
	if tc.ToolName != "shell_command" || tc.Category != "bash" {
		t.Errorf("tool call = %+v", tc)
	}
	if tc.ResultBody != "ok" || tc.ResultStatus != "ok" {
		t.Errorf("tool result = %q (%s)", tc.ResultBody, tc.ResultStatus)
	}
	if tc.ResultBytes != 2 || tc.ResultMediaType != "text/plain" {
		t.Errorf("tool result size = %d (%s), want 2 text/plain", tc.ResultBytes, tc.ResultMediaType)
	}
	if tc.MessageOrdinal != 1 {
		t.Errorf("tool call ordinal = %d, want 1", tc.MessageOrdinal)
	}

	if len(s.UsageEvent) != 2 {
		t.Fatalf("usage events = %d, want 2", len(s.UsageEvent))
	}
	u := s.UsageEvent[0]
	// Codex reports a combined input; cached must be split out.
	if u.Input != 600 || u.CacheRead != 400 || u.Output != 200 || u.Reasoning != 50 {
		t.Errorf("usage 0 = %+v", u)
	}
	if u.Model != "gpt-5-codex" {
		t.Errorf("usage 0 model = %q", u.Model)
	}
	if u.MessageOrdinal == nil || *u.MessageOrdinal != 1 {
		t.Errorf("usage 0 ordinal = %v", u.MessageOrdinal)
	}
	// The second turn's usage must attach to the second assistant turn, not the
	// first: this is the regression guard for the lastAssistant reset.
	u2 := s.UsageEvent[1]
	if u2.Input != 400 || u2.CacheRead != 100 || u2.Output != 100 {
		t.Errorf("usage 1 = %+v", u2)
	}
	if u2.MessageOrdinal == nil || *u2.MessageOrdinal != 3 {
		t.Errorf("usage 1 ordinal = %v, want 3", u2.MessageOrdinal)
	}
}

func TestParsePi(t *testing.T) {
	s, err := Parse(AgentPi, loadFixture(t, "pi.jsonl"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if s.Cwd != "/home/grace/code/proj" {
		t.Errorf("cwd = %q", s.Cwd)
	}

	if len(s.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(s.Messages))
	}
	if s.Messages[0].Role != RoleUser || s.Messages[0].Content != "Fix the login bug" {
		t.Errorf("message 0 = %+v", s.Messages[0])
	}
	a := s.Messages[1]
	if a.Content != "Looking at auth." || a.Model != "claude-opus-4-20250514" {
		t.Errorf("message 1 = %+v", a)
	}
	if !a.HasThinking || a.ThinkingText != "Inspect the auth package" {
		t.Errorf("message 1 thinking = %q (has=%v)", a.ThinkingText, a.HasThinking)
	}
	if !a.HasToolUse {
		t.Errorf("message 1 should have tool use")
	}

	if len(s.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(s.ToolCalls))
	}
	tc := s.ToolCalls[0]
	if tc.ToolName != "read" || tc.Category != "read" || tc.FilePath != "auth.go" {
		t.Errorf("tool call = %+v", tc)
	}
	if tc.ResultBody != "package auth" || tc.ResultStatus != "ok" {
		t.Errorf("tool result = %q (%s)", tc.ResultBody, tc.ResultStatus)
	}
	// pi delivers the result as an array of text blocks; the body is flattened to
	// readable text, and its size and media type describe that flattened content
	// exactly (so the CAS blob matches the recorded metadata).
	if tc.ResultMediaType != "text/plain" || tc.ResultBytes != len("package auth") {
		t.Errorf("tool result size = %d (%s), want %d (text/plain)", tc.ResultBytes, tc.ResultMediaType, len("package auth"))
	}

	if len(s.UsageEvent) != 1 {
		t.Fatalf("usage events = %d, want 1", len(s.UsageEvent))
	}
	u := s.UsageEvent[0]
	if u.Input != 100 || u.Output != 50 || u.Model != "claude-opus-4-20250514" {
		t.Errorf("usage = %+v", u)
	}
	if u.DedupKey != "e2" {
		t.Errorf("usage dedup = %q", u.DedupKey)
	}
}

// TestParseUnknownAgent confirms an unrecognized agent is a hard error, not a
// silently empty session.
func TestParseUnknownAgent(t *testing.T) {
	if _, err := Parse(Agent("nope"), []byte("{}")); err == nil {
		t.Fatal("expected error for unknown agent")
	}
}

// TestParseSkipsMalformedLines confirms a single bad line does not abort the
// rest of the session.
func TestParseSkipsMalformedLines(t *testing.T) {
	raw := []byte(`{"type":"user","message":{"content":"hello"}}
this is not json
{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}`)
	s, err := Parse(AgentClaude, raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(s.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(s.Messages))
	}
}
