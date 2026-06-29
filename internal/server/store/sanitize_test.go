package store

import (
	"context"
	"testing"
)

// TestSanitizeText pins the contract the projection write boundary relies on:
// valid text passes through untouched, a NUL byte and an invalid UTF-8 byte each
// become U+FFFD, and the two faults can coexist in one string. It needs no
// database, so it runs even when the integration env var is unset.
func TestSanitizeText(t *testing.T) {
	const repl = "�"
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain ascii", "Grace Hopper found the bug", "Grace Hopper found the bug"},
		{"valid multibyte", "Ada Lovelace café résumé", "Ada Lovelace café résumé"},
		{"bare nul", "\x00", repl},
		{"nul in the middle", "before\x00after", "before" + repl + "after"},
		{"invalid utf8 byte", "bad\xffbyte", "bad" + repl + "byte"},
		{"lone continuation byte", "x\x80y", "x" + repl + "y"},
		{"nul and invalid together", "mix\x00\xffend", "mix" + repl + repl + "end"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeText(c.in); got != c.want {
				t.Fatalf("sanitizeText(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestApplyDeltaSanitizesText drives a message body carrying a raw NUL and an
// invalid UTF-8 sequence, plus a tool call with the same faults in its name and
// file path, through the projection write that previously failed the reparse of
// Claude session 504. The INSERT must now succeed and the rows must read back
// with the offending bytes shown as U+FFFD, proving the seam covers every text
// column rather than only the one that first tripped.
func TestApplyDeltaSanitizesText(t *testing.T) {
	t.Parallel()
	st := newTestStore(t)
	ctx := context.Background()

	u, err := st.Register(ctx, "grace", "hash", "")
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := st.UpsertProject(ctx, "github.com/jssblck/akari", "github.com", "jssblck", "akari", "akari", "remote")
	if err != nil {
		t.Fatal(err)
	}
	sid := seedSession(t, st, u.ID, projectID, "sess-nul")

	const repl = "�"
	delta := ProjectionDelta{
		Messages: []MessageDelta{{
			Ordinal:      0,
			Role:         "assistant",
			Content:      "Grace Hopper\x00 traced the moth",
			ThinkingText: "reasoning\xff about it",
			Model:        "claude",
			HasThinking:  true,
			HasToolUse:   true,
		}},
		ToolCalls: []ProjToolCall{{
			MessageOrdinal: 0,
			CallIndex:      0,
			ToolName:       "Re\x00ad",
			Category:       "read",
			FilePath:       "src/\xffauth.ts",
			InputBody:      `{"file_path":"src/auth.ts"}`,
			InputMediaType: "application/json",
			CallUID:        "call-1",
		}},
	}
	if err := st.ApplyProjectionDelta(ctx, sid, delta); err != nil {
		t.Fatalf("apply delta with NUL and invalid UTF-8: %v", err)
	}

	msgs, err := st.Messages(ctx, sid)
	if err != nil {
		t.Fatalf("read messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("read %d messages, want 1", len(msgs))
	}
	if got, want := msgs[0].Content, "Grace Hopper"+repl+" traced the moth"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if got, want := msgs[0].ThinkingText, "reasoning"+repl+" about it"; got != want {
		t.Errorf("thinking_text = %q, want %q", got, want)
	}

	calls, err := st.ToolCalls(ctx, sid)
	if err != nil {
		t.Fatalf("read tool calls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("read %d tool calls, want 1", len(calls))
	}
	if got, want := calls[0].ToolName, "Re"+repl+"ad"; got != want {
		t.Errorf("tool_name = %q, want %q", got, want)
	}
	if got, want := calls[0].FilePath, "src/"+repl+"auth.ts"; got != want {
		t.Errorf("file_path = %q, want %q", got, want)
	}
}
