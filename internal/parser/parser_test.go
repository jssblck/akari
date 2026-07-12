package parser

import (
	"os"
	"path/filepath"
	"strings"
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
	// A Bash call has no file_path, so the inline parse derives its detail from the
	// command for the UI to show in its place.
	if tc.Detail != "false" {
		t.Errorf("detail = %q, want %q", tc.Detail, "false")
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

// TestParseCodexContext pins both ways Codex framing becomes context. Two opening
// user turns make the first context even when a new injected block precedes the
// known markers. Marker matching still catches framing re-injected after compaction.
func TestParseCodexContext(t *testing.T) {
	s, err := Parse(AgentCodex, loadFixture(t, "codex_context.jsonl"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// developer instructions are dropped; the two framing turns and two prompts and one
	// assistant reply remain: context, user, assistant, context, user.
	if len(s.Messages) != 5 {
		t.Fatalf("messages = %d, want 5", len(s.Messages))
	}

	// Codex can add new blocks before AGENTS.md. The consecutive opening user turns
	// provide the durable signal that the first turn is framing.
	if s.Messages[0].Role != RoleContext {
		t.Errorf("message 0 role = %q, want context", s.Messages[0].Role)
	}
	if !strings.HasPrefix(s.Messages[0].Content, "<recommended_plugins>") {
		t.Errorf("message 0 content = %q", s.Messages[0].Content)
	}

	// The real first prompt follows and keeps the user role, so it is the session's opener.
	if s.Messages[1].Role != RoleUser || s.Messages[1].Content != "Add rate limiting" {
		t.Errorf("message 1 = %+v", s.Messages[1])
	}
	if s.Messages[2].Role != RoleAssistant {
		t.Errorf("message 2 role = %q, want assistant", s.Messages[2].Role)
	}

	// The environment block re-injected after a compaction is context too, matched by content
	// rather than position, and the prompt that follows it is still a user turn.
	if s.Messages[3].Role != RoleContext {
		t.Errorf("message 3 role = %q, want context", s.Messages[3].Role)
	}
	if s.Messages[4].Role != RoleUser || s.Messages[4].Content != "Now add tests" {
		t.Errorf("message 4 = %+v", s.Messages[4])
	}

	// Exactly two turns are real human prompts; the two framing turns are excluded.
	users := 0
	for _, m := range s.Messages {
		if m.Role == RoleUser {
			users++
		}
	}
	if users != 2 {
		t.Errorf("user messages = %d, want 2", users)
	}
}

func TestParseCodexOpeningUserPairPromotesFirstWithoutMarkers(t *testing.T) {
	raw := []byte(
		`{"type":"response_item","timestamp":"2024-02-01T09:00:00Z","payload":{"role":"user","content":[{"type":"input_text","text":"future injected framing with no known marker"}]}}` + "\n" +
			`{"type":"response_item","timestamp":"2024-02-01T09:00:01Z","payload":{"role":"user","content":[{"type":"input_text","text":"Add rate limiting"}]}}` + "\n" +
			`{"type":"response_item","timestamp":"2024-02-01T09:00:08Z","payload":{"role":"assistant","content":[{"type":"output_text","text":"On it."}]}}` + "\n",
	)

	s, err := Parse(AgentCodex, raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(s.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(s.Messages))
	}
	if s.Messages[0].Role != RoleContext {
		t.Errorf("message 0 role = %q, want context", s.Messages[0].Role)
	}
	if s.Messages[1].Role != RoleUser || s.Messages[1].Content != "Add rate limiting" {
		t.Errorf("message 1 = %+v", s.Messages[1])
	}
}

func TestIsCodexContext(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{
			name: "new preamble before environment",
			text: "prefix\n<recommended_plugins>plugins</recommended_plugins>\nsuffix\n<environment_context>cwd</environment_context>",
			want: true,
		},
		{
			name: "agents instructions after other text",
			text: "unrecognized preamble\n# AGENTS.md instructions for /home/ada/project\n<environment_context>cwd</environment_context>",
			want: true,
		},
		{
			name: "legacy user instructions paired with preamble",
			text: "<user_instructions>Run tests.</user_instructions>\n<recommended_plugins>plugins</recommended_plugins>",
			want: true,
		},
		{
			name: "one marker",
			text: "please explain <environment_context> in this prompt",
			want: false,
		},
		{
			name: "same marker repeated",
			text: "<environment_context>one</environment_context>\n<environment_context>two</environment_context>",
			want: false,
		},
		{name: "ordinary prompt", text: "Add rate limiting to the ingest endpoint", want: false},
		{name: "empty", text: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCodexContext(tt.text); got != tt.want {
				t.Errorf("isCodexContext() = %v, want %v", got, tt.want)
			}
		})
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

// TestParseClaudeFallbackFromBlockAndIterations covers the first-message-fell-back shape:
// several assistant entries share a message id and requestId, one carries the "fallback"
// content block and all carry usage.iterations with a fallback_message entry. The reducer
// emits one FallbackOp per entry (the store dedups them by requestId), each reading the
// declined attempt's summed tokens from the type=="message" iteration entry and the served
// model from the fallback_message entry.
func TestParseClaudeFallbackFromBlockAndIterations(t *testing.T) {
	iters := `"iterations":[{"input_tokens":7626,"output_tokens":5,"cache_read_input_tokens":21535,"cache_creation_input_tokens":9698,"type":"message","model":"claude-fable-5"},{"input_tokens":7626,"output_tokens":214,"cache_read_input_tokens":31233,"cache_creation_input_tokens":0,"type":"fallback_message","model":"claude-opus-4-8"}]`
	raw := []byte(`{"type":"assistant","timestamp":"2026-07-02T07:42:34Z","requestId":"req_1","message":{"id":"msg_x","model":"claude-opus-4-8","content":[{"type":"fallback","from":{"model":"claude-fable-5"},"to":{"model":"claude-opus-4-8"}}],"usage":{"input_tokens":7626,"output_tokens":214,` + iters + `}}}
{"type":"assistant","timestamp":"2026-07-02T07:42:36Z","requestId":"req_1","message":{"id":"msg_x","model":"claude-opus-4-8","content":[{"type":"text","text":"391"}],"usage":{"input_tokens":7626,"output_tokens":214,` + iters + `}}}
`)
	s, err := Parse(AgentClaude, raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// Two assistant entries, so two ops, both keyed on the shared requestId: the store's
	// merge collapses them to one logical event. The reducer itself does not dedup.
	if len(s.Fallbacks) != 2 {
		t.Fatalf("fallback ops = %d, want 2 (one per assistant entry, deduped downstream)", len(s.Fallbacks))
	}
	for i, f := range s.Fallbacks {
		if f.DedupKey != "req_1" {
			t.Errorf("op %d dedup key = %q, want req_1", i, f.DedupKey)
		}
		if f.FromModel != "claude-fable-5" || f.ToModel != "claude-opus-4-8" {
			t.Errorf("op %d models: from=%q to=%q", i, f.FromModel, f.ToModel)
		}
		if f.DeclinedInput != 7626 || f.DeclinedOutput != 5 || f.DeclinedCacheWrite != 9698 || f.DeclinedCacheRead != 21535 {
			t.Errorf("op %d declined tokens = %+v", i, f)
		}
		if !f.DeclinedObserved {
			t.Errorf("op %d summed declined tokens, so DeclinedObserved should be true", i)
		}
		if f.MessageOrdinal == nil {
			t.Errorf("op %d should carry the ordinal of the message it rode", i)
		}
	}
	// The two entries share one message id, so they fold into one turn and both
	// ops tie to that turn's ordinal.
	for i, f := range s.Fallbacks {
		if got := *f.MessageOrdinal; got != 0 {
			t.Errorf("op %d ordinal = %d, want 0 (both entries fold into one turn)", i, got)
		}
	}
	if len(s.Messages) != 1 {
		t.Errorf("messages = %d, want 1 (the split entries fold by message id)", len(s.Messages))
	}
}

// TestParseClaudeFallbackDedupKeyFallsBackToMessageID covers the dedup key when the entry
// has no top-level requestId: it falls back to the assistant message id, so the store still
// merges every line of one logical fallback (Claude repeats the message id across the split
// entries even when requestId is absent).
func TestParseClaudeFallbackDedupKeyFallsBackToMessageID(t *testing.T) {
	raw := []byte(`{"type":"assistant","timestamp":"2026-07-02T11:30:00Z","message":{"id":"msg_noreq","model":"claude-opus-4-8","content":[{"type":"fallback","from":{"model":"claude-fable-5"},"to":{"model":"claude-opus-4-8"}}]}}
`)
	s, err := Parse(AgentClaude, raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(s.Fallbacks) != 1 {
		t.Fatalf("fallback ops = %d, want 1", len(s.Fallbacks))
	}
	if got := s.Fallbacks[0].DedupKey; got != "msg_noreq" {
		t.Errorf("dedup key = %q, want the message id msg_noreq (no requestId present)", got)
	}
}

// TestParseClaudeFallbackSystemEntry covers the system model_refusal_fallback entry: it
// carries the refusal trigger, category, and (possibly null) explanation, shares the
// assistant entry's requestId as its dedup key, and produces no message row so it does not
// disturb ordinals. It is the only system subtype the reducer keeps.
func TestParseClaudeFallbackSystemEntry(t *testing.T) {
	raw := []byte(`{"type":"system","subtype":"model_refusal_fallback","trigger":"refusal","originalModel":"claude-fable-5","fallbackModel":"claude-opus-4-8","requestId":"req_1","apiRefusalCategory":"reasoning_extraction","apiRefusalExplanation":null,"timestamp":"2026-07-02T07:42:37Z"}
{"type":"system","subtype":"some_other_notice","timestamp":"2026-07-02T07:42:38Z","content":"ignored"}
`)
	s, err := Parse(AgentClaude, raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(s.Messages) != 0 {
		t.Fatalf("system entries produce no message rows, got %d", len(s.Messages))
	}
	if len(s.Fallbacks) != 1 {
		t.Fatalf("fallback ops = %d, want 1 (only model_refusal_fallback is kept)", len(s.Fallbacks))
	}
	f := s.Fallbacks[0]
	if f.DedupKey != "req_1" {
		t.Errorf("dedup key = %q, want req_1", f.DedupKey)
	}
	if f.Trigger != "refusal" || f.RefusalCategory != "reasoning_extraction" {
		t.Errorf("refusal fields: trigger=%q category=%q", f.Trigger, f.RefusalCategory)
	}
	if f.RefusalExplanation != "" {
		t.Errorf("null explanation should read empty, got %q", f.RefusalExplanation)
	}
	if f.FromModel != "claude-fable-5" || f.ToModel != "claude-opus-4-8" {
		t.Errorf("models: from=%q to=%q", f.FromModel, f.ToModel)
	}
	if f.MessageOrdinal != nil {
		t.Errorf("system op must carry no ordinal, got %v", *f.MessageOrdinal)
	}
}

// TestParseClaudeStickyFallbackNoBlock covers the sticky-served shape: a fallback_message
// iteration entry is present but there is NO fallback content block. Detection must key on
// the iterations independently of the block, so the op is still emitted, reading FromModel
// from the type=="message" iteration entry and ToModel from the fallback_message entry.
func TestParseClaudeStickyFallbackNoBlock(t *testing.T) {
	raw := []byte(`{"type":"assistant","timestamp":"2026-07-02T08:00:00Z","requestId":"req_sticky","message":{"id":"msg_s","model":"claude-opus-4-8","content":[{"type":"text","text":"still on opus"}],"usage":{"input_tokens":10,"output_tokens":20,"iterations":[{"input_tokens":10,"output_tokens":3,"cache_read_input_tokens":40,"cache_creation_input_tokens":5,"type":"message","model":"claude-fable-5"},{"input_tokens":10,"output_tokens":20,"type":"fallback_message","model":"claude-opus-4-8"}]}}}
`)
	s, err := Parse(AgentClaude, raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(s.Fallbacks) != 1 {
		t.Fatalf("fallback ops = %d, want 1 (iterations detected without a block)", len(s.Fallbacks))
	}
	f := s.Fallbacks[0]
	if f.FromModel != "claude-fable-5" || f.ToModel != "claude-opus-4-8" {
		t.Errorf("models: from=%q to=%q", f.FromModel, f.ToModel)
	}
	if f.DeclinedInput != 10 || f.DeclinedOutput != 3 || f.DeclinedCacheWrite != 5 || f.DeclinedCacheRead != 40 {
		t.Errorf("declined tokens = %+v", f)
	}
	if !f.DeclinedObserved {
		t.Error("a sticky fallback summed its iteration entries, so DeclinedObserved should be true")
	}
	if f.DedupKey != "req_sticky" {
		t.Errorf("dedup key = %q", f.DedupKey)
	}
}

// TestParseClaudeFallbackBlockNoIterations covers a fallback whose declined spend was never
// reported: an assistant entry carries a "fallback" content block but no usage.iterations.
// It is still detected as a fallback (the block is an explicit marker), but the declined
// counts stay zero AND DeclinedObserved stays false, so a downstream reader can tell an
// unmeasured attempt from a real zero-token one and leave the store column NULL.
func TestParseClaudeFallbackBlockNoIterations(t *testing.T) {
	raw := []byte(`{"type":"assistant","timestamp":"2026-07-02T10:00:00Z","requestId":"req_blk","message":{"id":"msg_b","model":"claude-opus-4-8","content":[{"type":"fallback","from":{"model":"claude-fable-5"},"to":{"model":"claude-opus-4-8"}}]}}
`)
	s, err := Parse(AgentClaude, raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(s.Fallbacks) != 1 {
		t.Fatalf("fallback ops = %d, want 1 (the block is an explicit marker)", len(s.Fallbacks))
	}
	f := s.Fallbacks[0]
	if f.FromModel != "claude-fable-5" || f.ToModel != "claude-opus-4-8" {
		t.Errorf("models: from=%q to=%q", f.FromModel, f.ToModel)
	}
	if f.DeclinedObserved {
		t.Error("no iterations means the declined spend was never observed, so DeclinedObserved must be false")
	}
	if f.DeclinedInput != 0 || f.DeclinedOutput != 0 || f.DeclinedCacheWrite != 0 || f.DeclinedCacheRead != 0 {
		t.Errorf("unobserved declined counts should stay zero, got %+v", f)
	}
	if f.MessageOrdinal == nil {
		t.Error("an assistant-side op still rides its message ordinal")
	}
}

// TestParseClaudeNoFallbackNegativeControls confirms detection never fires without an
// explicit marker: (a) an assistant turn whose iterations is a single type=="message" entry
// with the model absent (an ordinary turn, the shape the spec warns reads like a fallback if
// you key on iterations blindly), and (b) two assistant turns with different model strings and
// no markers (an intentional model switch). Neither produces a fallback op.
func TestParseClaudeNoFallbackNegativeControls(t *testing.T) {
	// (a) An ordinary turn carrying iterations with one type=="message" entry, model absent.
	ordinary := `{"type":"assistant","timestamp":"2026-07-02T09:00:00Z","requestId":"req_a","message":{"id":"m_a","model":"claude-fable-5","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":5,"output_tokens":5,"iterations":[{"input_tokens":5,"output_tokens":5,"cache_creation_input_tokens":1,"type":"message"}]}}}` + "\n"
	// (b) A later turn on a different model, no markers at all: an intentional /model switch.
	switched := `{"type":"assistant","timestamp":"2026-07-02T09:01:00Z","requestId":"req_b","message":{"id":"m_b","model":"claude-opus-4-8","content":[{"type":"text","text":"switched"}],"usage":{"input_tokens":5,"output_tokens":5}}}` + "\n"

	s, err := Parse(AgentClaude, []byte(ordinary+switched))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(s.Fallbacks) != 0 {
		t.Fatalf("no explicit marker means no fallback op, got %d: %+v", len(s.Fallbacks), s.Fallbacks)
	}
	// Both turns still parse as ordinary messages.
	if len(s.Messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(s.Messages))
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
