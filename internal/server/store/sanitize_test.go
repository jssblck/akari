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

// TestApplyDeltaSanitizesText drives a NUL and an invalid UTF-8 byte through
// every text column applyDelta writes: the message body that first failed the
// reparse of Claude session 504, and also the columns the same seam now covers
// (message role and model, tool call name/category/file_path/input_media_type,
// the back-patched tool result, usage model and dedup_key, and attachment
// filename and media_type). The whole INSERT/UPDATE set must now succeed and
// each row must read back with the offending bytes shown as U+FFFD. call_uid is
// not exposed in any read view, so its sanitization is proven indirectly: the
// tool result carries the same NUL-bearing call_uid as the call, and the
// back-patch lands only because the insert and the update sanitize it
// identically, so the result columns reading back as U+FFFD show the match held.
func TestApplyDeltaSanitizesText(t *testing.T) {
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
	image := []byte("\x89PNG generated image bytes")
	delta := ProjectionDelta{
		Messages: []MessageDelta{{
			Ordinal:      0,
			Role:         "assist\x00ant",
			Content:      "Grace Hopper\x00 traced the moth",
			ThinkingText: "reasoning\xff about it",
			Model:        "cla\x00ude",
			HasThinking:  true,
			HasToolUse:   true,
		}},
		ToolCalls: []ProjToolCall{{
			MessageOrdinal: 0,
			CallIndex:      0,
			ToolName:       "Re\x00ad",
			Category:       "re\xffad",
			FilePath:       "src/\xffauth.ts",
			InputBody:      `{"file_path":"src/auth.ts"}`,
			InputMediaType: "applica\x00tion/json",
			CallUID:        "call\x00-1",
		}},
		ToolResults: []ToolResultDelta{{
			CallUID:   "call\x00-1",
			Body:      "ok output",
			Bytes:     int64(len("ok output")),
			MediaType: "text/\xffplain",
			Status:    "suc\x00cess",
		}},
		Usage: []ProjUsage{{
			Model:        "cla\x00ude",
			Input:        5,
			DedupKey:     "msg\x00_1",
			SourceOffset: 0,
			SourceIndex:  0,
		}},
		Attachments: []AttachmentDelta{{
			MessageOrdinal: 0,
			Body:           string(image),
			Bytes:          int64(len(image)),
			MediaType:      "image/\xffpng",
			Filename:       "kit\x00ten.png",
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
	for _, c := range []struct{ field, got, want string }{
		{"role", msgs[0].Role, "assist" + repl + "ant"},
		{"content", msgs[0].Content, "Grace Hopper" + repl + " traced the moth"},
		{"thinking_text", msgs[0].ThinkingText, "reasoning" + repl + " about it"},
		{"model", msgs[0].Model, "cla" + repl + "ude"},
	} {
		if c.got != c.want {
			t.Errorf("message %s = %q, want %q", c.field, c.got, c.want)
		}
	}

	calls, err := st.ToolCalls(ctx, sid)
	if err != nil {
		t.Fatalf("read tool calls: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("read %d tool calls, want 1", len(calls))
	}
	for _, c := range []struct{ field, got, want string }{
		{"tool_name", calls[0].ToolName, "Re" + repl + "ad"},
		{"category", calls[0].Category, "re" + repl + "ad"},
		{"file_path", calls[0].FilePath, "src/" + repl + "auth.ts"},
		{"input_media_type", calls[0].InputMediaType, "applica" + repl + "tion/json"},
		// Set only if the result back-patch matched the NUL-bearing call_uid.
		{"result_media_type", calls[0].ResultMediaType, "text/" + repl + "plain"},
		{"result_status", calls[0].ResultStatus, "suc" + repl + "cess"},
	} {
		if c.got != c.want {
			t.Errorf("tool call %s = %q, want %q", c.field, c.got, c.want)
		}
	}

	var usageModel, dedupKey string
	if err := st.Pool.QueryRow(ctx,
		"SELECT model, dedup_key FROM usage_events WHERE session_id=$1", sid).
		Scan(&usageModel, &dedupKey); err != nil {
		t.Fatalf("read usage event: %v", err)
	}
	if want := "cla" + repl + "ude"; usageModel != want {
		t.Errorf("usage model = %q, want %q", usageModel, want)
	}
	if want := "msg" + repl + "_1"; dedupKey != want {
		t.Errorf("usage dedup_key = %q, want %q", dedupKey, want)
	}

	atts, err := st.Attachments(ctx, sid)
	if err != nil {
		t.Fatalf("read attachments: %v", err)
	}
	if len(atts) != 1 {
		t.Fatalf("read %d attachments, want 1", len(atts))
	}
	if want := "image/" + repl + "png"; atts[0].MediaType != want {
		t.Errorf("attachment media_type = %q, want %q", atts[0].MediaType, want)
	}
	if want := "kit" + repl + "ten.png"; atts[0].Filename != want {
		t.Errorf("attachment filename = %q, want %q", atts[0].Filename, want)
	}
}
