package parser

import (
	"strings"

	"github.com/tidwall/gjson"
)

// reduceClaude advances a Claude Code session over one raw region. Each line is a
// typed entry; user and assistant entries carry content as a string or an array
// of typed blocks (text, thinking, tool_use, tool_result). Token usage rides on
// the assistant entry. Tool results arrive as blocks in a following user entry
// and are back-patched to their tool_use by id.
func (r *reducer) reduceClaude(region []byte, base int64) error {
	return eachLine(region, base, func(line []byte, offset int64) error {
		if !gjson.ValidBytes(line) {
			return nil
		}
		e := gjson.ParseBytes(line)
		typ := e.Get("type").String()
		ts := parseTime(e.Get("timestamp").String())
		r.observe(ts)

		if cwd := e.Get("cwd").String(); cwd != "" {
			r.d.Cwd = cwd
		}
		if br := e.Get("gitBranch").String(); br != "" {
			r.d.GitBranch = br
		}

		switch typ {
		case "user":
			content := e.Get("message.content")
			text := blockText(content)
			if content.IsArray() {
				for _, b := range content.Array() {
					if b.Get("type").String() == "tool_result" {
						r.applyResult(b.Get("tool_use_id").String(), b.Get("content"), b.Get("is_error").Bool())
					}
				}
			}
			if strings.TrimSpace(text) == "" {
				return nil // a turn that only delivers tool results is not a message
			}
			r.addUser(text, ts)

		case "assistant":
			msg := e.Get("message")
			ord := r.st.NextOrdinal
			r.st.NextOrdinal++
			op := MessageOp{Ordinal: ord, Role: RoleAssistant, Model: msg.Get("model").String(), Timestamp: ts}
			var textParts, thinkParts []string
			callIndex := 0
			for _, b := range msg.Get("content").Array() {
				switch b.Get("type").String() {
				case "text":
					textParts = append(textParts, b.Get("text").String())
				case "thinking":
					thinkParts = append(thinkParts, b.Get("thinking").String())
				case "tool_use":
					op.HasToolUse = true
					name := b.Get("name").String()
					tc := ToolCall{
						MessageOrdinal: ord, CallIndex: callIndex,
						ToolName: name, Category: toolCategory(name),
						FilePath: b.Get("input.file_path").String(),
						CallUID:  b.Get("id").String(),
					}
					setToolInput(&tc, b.Get("input"), "application/json")
					r.d.ToolCalls = append(r.d.ToolCalls, tc)
					callIndex++
				}
			}
			op.Content = strings.Join(textParts, "\n")
			op.ThinkingText = strings.Join(thinkParts, "\n")
			op.HasThinking = op.ThinkingText != ""
			r.d.Messages = append(r.d.Messages, op)

			if u := msg.Get("usage"); u.Exists() {
				o := ord
				r.addUsage(Usage{
					MessageOrdinal: &o,
					Model:          op.Model,
					Input:          int(u.Get("input_tokens").Int()),
					Output:         int(u.Get("output_tokens").Int()),
					CacheWrite:     int(u.Get("cache_creation_input_tokens").Int()),
					CacheRead:      int(u.Get("cache_read_input_tokens").Int()),
					OccurredAt:     ts,
					DedupKey:       msg.Get("id").String(),
				}, offset)
			}
		}
		return nil
	})
}

// applyResult records a tool result against the call its id names. body is the
// raw result value (a string or an array of blocks). The stored body, its size,
// and its media type all come from bodyContent, so the recorded metadata always
// describes exactly the bytes the CAS holds.
func (r *reducer) applyResult(id string, body gjson.Result, isErr bool) {
	if id == "" {
		return // an unkeyed result cannot be matched to a call
	}
	status := "ok"
	if isErr {
		status = "error"
	}
	op := ToolResultOp{CallUID: id, Status: status}
	if ref, ok := asCASRef(body); ok {
		// The client already lifted this body to the CAS; record the reference and
		// its metadata, but do not carry a body for the server to re-store.
		op.BodySHA256, op.Bytes, op.MediaType = ref.SHA256, ref.Bytes, ref.MediaType
	} else {
		content, media := bodyContent(body)
		op.Body, op.Bytes, op.MediaType = content, len(content), media
	}
	r.d.ToolResults = append(r.d.ToolResults, op)
}

// setToolInput records a tool call's input, recognizing a CAS sentinel. When the
// client lifted the input to the CAS, the reference and its metadata are recorded
// and no inline body is carried; the sentinel's file_path and detail (the input
// fields the lift would otherwise erase) fill the call's FilePath and Detail.
// Otherwise the raw input JSON travels inline, the server hashes and sizes it, and
// the detail is derived here from the raw input. defaultMedia is the media type
// used for an inline body (every agent's tool input is JSON).
func setToolInput(tc *ToolCall, input gjson.Result, defaultMedia string) {
	if ref, ok := asCASRef(input); ok {
		tc.InputSHA256, tc.InputBytes, tc.InputMediaType = ref.SHA256, ref.Bytes, ref.MediaType
		if ref.FilePath != "" {
			tc.FilePath = ref.FilePath
		}
		tc.Detail = ref.Detail
		return
	}
	tc.InputJSON = input.Raw
	if defaultMedia == "application/json" {
		tc.Detail = inputDetail(input.Raw)
	}
}

// bodyContent returns the canonical body bytes and media type for a raw tool
// body. A string is its unquoted contents; an array of typed blocks is flattened
// to its text (so a result that arrives as text blocks renders as readable text,
// not a JSON wrapper); a genuine object stays raw JSON. The returned string is
// exactly what the CAS stores, so its length is the recorded size.
func bodyContent(body gjson.Result) (string, string) {
	switch {
	case body.Type == gjson.String:
		return body.String(), "text/plain"
	case body.IsArray():
		return blockText(body), "text/plain"
	case body.IsObject():
		return body.Raw, "application/json"
	case body.Exists():
		return body.Raw, "text/plain"
	default:
		return "", ""
	}
}

// blockText flattens a content value that may be a plain string or an array of
// typed blocks into a single text string.
func blockText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	var parts []string
	for _, b := range content.Array() {
		if b.Type == gjson.String {
			parts = append(parts, b.String())
			continue
		}
		switch b.Get("type").String() {
		case "text", "output_text", "input_text":
			parts = append(parts, b.Get("text").String())
		}
	}
	return strings.Join(parts, "\n")
}
