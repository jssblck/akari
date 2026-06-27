package parser

import (
	"strings"

	"github.com/tidwall/gjson"
)

// parseClaude parses Claude Code JSONL. Each line is a typed entry; user and
// assistant entries carry message content as either a string or an array of
// typed blocks (text, thinking, tool_use, tool_result). Token usage rides on the
// assistant entry. Tool results arrive as blocks in the following user entry and
// are matched back to their tool_use by id.
func parseClaude(raw []byte) (Session, error) {
	var s Session
	var sp span
	ordinal := 0
	toolByID := map[string]int{} // tool_use id -> index in s.ToolCalls

	lines, err := scanLines(raw)
	if err != nil {
		return Session{}, err
	}
	for _, line := range lines {
		if !gjson.Valid(line) {
			continue
		}
		e := gjson.Parse(line)
		typ := e.Get("type").String()
		ts := parseTime(e.Get("timestamp").String())
		sp.observe(ts)

		if cwd := e.Get("cwd").String(); cwd != "" {
			s.Cwd = cwd
		}
		if br := e.Get("gitBranch").String(); br != "" {
			s.GitBranch = br
		}

		switch typ {
		case "user":
			content := e.Get("message.content")
			text := blockText(content)
			// Apply any tool_result blocks to their originating tool calls.
			if content.IsArray() {
				for _, b := range content.Array() {
					if b.Get("type").String() == "tool_result" {
						applyToolResult(&s, toolByID,
							b.Get("tool_use_id").String(), b.Get("content"), b.Get("is_error").Bool())
					}
				}
			}
			if strings.TrimSpace(text) == "" {
				continue // a turn that only delivers tool results is not a message
			}
			s.Messages = append(s.Messages, Message{
				Ordinal: ordinal, Role: RoleUser, Content: text, Timestamp: ts,
			})
			ordinal++

		case "assistant":
			msg := e.Get("message")
			m := Message{Ordinal: ordinal, Role: RoleAssistant, Model: msg.Get("model").String(), Timestamp: ts}
			var textParts, thinkParts []string
			callIndex := 0
			for _, b := range msg.Get("content").Array() {
				switch b.Get("type").String() {
				case "text":
					textParts = append(textParts, b.Get("text").String())
				case "thinking":
					thinkParts = append(thinkParts, b.Get("thinking").String())
				case "tool_use":
					m.HasToolUse = true
					name := b.Get("name").String()
					tc := ToolCall{
						MessageOrdinal: ordinal, CallIndex: callIndex,
						ToolName: name, Category: toolCategory(name),
						FilePath:  b.Get("input.file_path").String(),
						InputJSON: b.Get("input").Raw,
					}
					if id := b.Get("id").String(); id != "" {
						toolByID[id] = len(s.ToolCalls)
					}
					s.ToolCalls = append(s.ToolCalls, tc)
					callIndex++
				}
			}
			m.Content = strings.Join(textParts, "\n")
			m.ThinkingText = strings.Join(thinkParts, "\n")
			m.HasThinking = m.ThinkingText != ""
			s.Messages = append(s.Messages, m)

			if u := msg.Get("usage"); u.Exists() {
				ord := ordinal
				s.UsageEvent = append(s.UsageEvent, Usage{
					MessageOrdinal: &ord,
					Model:          m.Model,
					Input:          int(u.Get("input_tokens").Int()),
					Output:         int(u.Get("output_tokens").Int()),
					CacheWrite:     int(u.Get("cache_creation_input_tokens").Int()),
					CacheRead:      int(u.Get("cache_read_input_tokens").Int()),
					OccurredAt:     ts,
					DedupKey:       msg.Get("id").String(),
				})
			}
			ordinal++
		}
	}

	s.StartedAt, s.EndedAt = sp.started, sp.ended
	return s, nil
}

// applyToolResult records a tool result against the matching tool call. body is
// the raw result value (a string or an array of blocks); its size and media type
// are taken from the original body so the recorded metadata is faithful, while
// ResultText holds a flattened form for display and search.
func applyToolResult(s *Session, toolByID map[string]int, id string, body gjson.Result, isErr bool) {
	if id == "" {
		return // an unkeyed result cannot be matched to a call
	}
	idx, ok := toolByID[id]
	if !ok {
		return
	}
	tc := &s.ToolCalls[idx]
	tc.ResultText = blockText(body)
	tc.ResultBytes, tc.ResultMediaType = bodySize(body)
	if isErr {
		tc.ResultStatus = "error"
	} else {
		tc.ResultStatus = "ok"
	}
}

// bodySize reports the byte length and media type of a raw tool body. Strings are
// measured by their unquoted contents (text/plain); arrays and objects by their
// JSON encoding (application/json). This is the size the CAS will later store.
func bodySize(body gjson.Result) (int, string) {
	switch {
	case body.Type == gjson.String:
		return len(body.String()), "text/plain"
	case body.IsArray() || body.IsObject():
		return len(body.Raw), "application/json"
	case body.Exists():
		return len(body.Raw), "text/plain"
	default:
		return 0, ""
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
