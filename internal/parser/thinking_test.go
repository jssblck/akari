package parser

import (
	"strings"
	"testing"
)

// The observed-thinking signal weighs each turn's reasoning by its byte size, and the
// whole point is that this survives redaction: current Claude Code and Codex encrypt the
// reasoning and drop the plaintext, so a turn with empty thinking text must still register
// its volume through the encrypted payload. These tests pin that capture at the reducer,
// the seam the original implementation missed by keying HasThinking on non-empty text.

// TestClaudeRedactedThinkingWeight drives a Claude turn whose thinking block is redacted
// to empty text with only a signature (the shape the current client emits), plus a
// redacted_thinking block and a rare block that kept its plaintext, and checks the weight
// sums every reasoning byte while the text carries only what survived.
func TestClaudeRedactedThinkingWeight(t *testing.T) {
	sig := strings.Repeat("s", 400)  // encrypted thinking behind a redacted block
	data := strings.Repeat("d", 250) // a fully redacted_thinking blob
	kept := "Consider the auth module"
	sig2 := strings.Repeat("t", 120)
	line := `{"type":"assistant","timestamp":"2024-01-01T10:00:00Z","message":{"model":"claude-opus-4-8","content":[` +
		`{"type":"thinking","thinking":"","signature":"` + sig + `"},` +
		`{"type":"redacted_thinking","data":"` + data + `"},` +
		`{"type":"thinking","thinking":"` + kept + `","signature":"` + sig2 + `"},` +
		`{"type":"text","text":"Done."}]}}` + "\n"

	s, err := Parse(AgentClaude, []byte(line))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(s.Messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(s.Messages))
	}
	a := s.Messages[0]
	if !a.HasThinking {
		t.Error("a turn with reasoning blocks must mark HasThinking even when the text is redacted")
	}
	if a.ThinkingText != kept {
		t.Errorf("thinking text = %q, want only the surviving plaintext %q", a.ThinkingText, kept)
	}
	want := len(sig) + len(data) + len(kept) + len(sig2)
	if a.ThinkingBytes != want {
		t.Errorf("thinking bytes = %d, want %d (every reasoning byte, redacted or not)", a.ThinkingBytes, want)
	}
}

// TestCodexEncryptedReasoningWeight drives a Codex turn whose reasoning is encrypted with
// an empty summary (the current shape) folded into the assistant message, and checks the
// weight is the ciphertext length while the turn reads as thinking.
func TestCodexEncryptedReasoningWeight(t *testing.T) {
	enc := strings.Repeat("e", 1600)
	region := `{"type":"response_item","timestamp":"2024-01-01T10:00:00Z","payload":{"type":"reasoning","summary":[],"encrypted_content":"` + enc + `"}}` + "\n" +
		`{"type":"response_item","timestamp":"2024-01-01T10:00:01Z","payload":{"role":"assistant","content":[{"type":"output_text","text":"done"}]}}` + "\n" +
		`{"type":"response_item","timestamp":"2024-01-01T10:00:02Z","payload":{"role":"user","content":[{"type":"input_text","text":"next"}]}}` + "\n"

	s, err := Parse(AgentCodex, []byte(region))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a := s.Messages[0]
	if a.Role != RoleAssistant || !a.HasThinking {
		t.Fatalf("first message should be an assistant turn that thought: %+v", a)
	}
	if a.ThinkingText != "" {
		t.Errorf("encrypted reasoning carries no text, got %q", a.ThinkingText)
	}
	if a.ThinkingBytes != len(enc) {
		t.Errorf("thinking bytes = %d, want %d (encrypted_content length)", a.ThinkingBytes, len(enc))
	}
}

// TestCodexReasoningSummaryWeight drives a Codex reasoning item that kept a plaintext
// summary alongside its ciphertext, and checks the weight sums both and the text is the
// summary.
func TestCodexReasoningSummaryWeight(t *testing.T) {
	enc := strings.Repeat("e", 900)
	summary := "Plan the shell command."
	region := `{"type":"response_item","timestamp":"2024-01-01T10:00:00Z","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"` + summary + `"}],"encrypted_content":"` + enc + `"}}` + "\n" +
		`{"type":"response_item","timestamp":"2024-01-01T10:00:01Z","payload":{"role":"assistant","content":[{"type":"output_text","text":"done"}]}}` + "\n" +
		`{"type":"response_item","timestamp":"2024-01-01T10:00:02Z","payload":{"role":"user","content":[{"type":"input_text","text":"next"}]}}` + "\n"

	s, err := Parse(AgentCodex, []byte(region))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a := s.Messages[0]
	if a.ThinkingText != summary {
		t.Errorf("thinking text = %q, want the summary %q", a.ThinkingText, summary)
	}
	if want := len(summary) + len(enc); a.ThinkingBytes != want {
		t.Errorf("thinking bytes = %d, want %d (summary + encrypted_content)", a.ThinkingBytes, want)
	}
}

// TestPiPlaintextThinkingWeight drives a pi turn, which keeps its thinking in the clear, and
// checks the weight is the plaintext length.
func TestPiPlaintextThinkingWeight(t *testing.T) {
	think := "Reviewing main and status before the diff."
	line := `{"type":"message","timestamp":"2024-01-01T10:00:00Z","message":{"role":"assistant","model":"pi-1","content":[` +
		`{"type":"thinking","thinking":"` + think + `"},{"type":"text","text":"Done."}]}}` + "\n"

	s, err := Parse(AgentPi, []byte(line))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a := s.Messages[0]
	if !a.HasThinking || a.ThinkingText != think {
		t.Fatalf("pi thinking = %q (has=%v)", a.ThinkingText, a.HasThinking)
	}
	if a.ThinkingBytes != len(think) {
		t.Errorf("thinking bytes = %d, want %d (plaintext length)", a.ThinkingBytes, len(think))
	}
}
