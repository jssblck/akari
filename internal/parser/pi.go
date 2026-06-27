package parser

import (
	"strings"

	"github.com/tidwall/gjson"
)

// parsePi parses pi JSONL. The first line is a session header carrying cwd;
// message lines carry a role (user, assistant, toolResult) with typed content
// blocks (text, thinking, toolCall), a model, and token usage on assistant
// turns.
func parsePi(raw []byte) (Session, error) {
	var s Session
	var sp span
	ordinal := 0
	toolByID := map[string]int{} // toolCall id -> index in s.ToolCalls

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

		switch typ {
		case "session":
			if cwd := e.Get("cwd").String(); cwd != "" {
				s.Cwd = cwd
			}

		case "message":
			msg := e.Get("message")
			switch msg.Get("role").String() {
			case "user":
				s.Messages = append(s.Messages, Message{
					Ordinal: ordinal, Role: RoleUser, Content: blockText(msg.Get("content")), Timestamp: ts,
				})
				ordinal++

			case "assistant":
				m := Message{Ordinal: ordinal, Role: RoleAssistant, Model: msg.Get("model").String(), Timestamp: ts}
				var textParts, thinkParts []string
				callIndex := 0
				for _, b := range msg.Get("content").Array() {
					switch b.Get("type").String() {
					case "text":
						textParts = append(textParts, b.Get("text").String())
					case "thinking":
						thinkParts = append(thinkParts, b.Get("thinking").String())
					case "toolCall":
						m.HasToolUse = true
						name := b.Get("name").String()
						tc := ToolCall{
							MessageOrdinal: ordinal, CallIndex: callIndex,
							ToolName: name, Category: toolCategory(name),
							FilePath:  b.Get("arguments.file_path").String(),
							InputJSON: b.Get("arguments").Raw,
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
						MessageOrdinal: &ord, Model: m.Model,
						Input:      int(u.Get("input").Int()),
						Output:     int(u.Get("output").Int()),
						OccurredAt: ts, DedupKey: e.Get("id").String(),
					})
				}
				ordinal++

			case "toolResult":
				applyToolResult(&s, toolByID,
					msg.Get("toolCallId").String(), msg.Get("content"), msg.Get("isError").Bool())
			}
		}
	}

	s.StartedAt, s.EndedAt = sp.started, sp.ended
	return s, nil
}
