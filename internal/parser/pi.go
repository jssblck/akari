package parser

import (
	"strings"

	"github.com/tidwall/gjson"
)

// reducePi advances a pi session over one raw region. The first line is a session
// header carrying cwd; message lines carry a role (user, assistant, toolResult)
// with typed content blocks (text, thinking, toolCall), a model, and token usage
// on assistant turns. Tool results are back-patched to their call by id.
func (r *reducer) reducePi(region []byte, base int64) error {
	return eachLine(region, base, func(line []byte, offset int64) error {
		if !gjson.ValidBytes(line) {
			return nil
		}
		e := gjson.ParseBytes(line)
		typ := e.Get("type").String()
		ts := parseTime(e.Get("timestamp").String())
		r.observe(ts)

		switch typ {
		case "session":
			if cwd := e.Get("cwd").String(); cwd != "" {
				r.d.Cwd = cwd
			}

		case "message":
			msg := e.Get("message")
			switch msg.Get("role").String() {
			case "user":
				r.addUser(blockText(msg.Get("content")), ts)

			case "assistant":
				ord := r.st.NextOrdinal
				r.st.NextOrdinal++
				r.d.MessagesAdded++
				op := MessageOp{Ordinal: ord, Role: RoleAssistant, Model: msg.Get("model").String(), Timestamp: ts}
				var textParts, thinkParts []string
				callIndex := 0
				for _, b := range msg.Get("content").Array() {
					switch b.Get("type").String() {
					case "text":
						textParts = append(textParts, b.Get("text").String())
					case "thinking":
						thinkParts = append(thinkParts, b.Get("thinking").String())
					case "toolCall":
						op.HasToolUse = true
						name := b.Get("name").String()
						r.d.ToolCalls = append(r.d.ToolCalls, ToolCall{
							MessageOrdinal: ord, CallIndex: callIndex,
							ToolName: name, Category: toolCategory(name),
							FilePath:  b.Get("arguments.file_path").String(),
							InputJSON: b.Get("arguments").Raw,
							CallUID:   b.Get("id").String(),
						})
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
						MessageOrdinal: &o, Model: op.Model,
						Input:      int(u.Get("input").Int()),
						Output:     int(u.Get("output").Int()),
						OccurredAt: ts, DedupKey: e.Get("id").String(),
					}, offset)
				}

			case "toolResult":
				r.applyResult(msg.Get("toolCallId").String(), msg.Get("content"), msg.Get("isError").Bool())
			}
		}
		return nil
	})
}
