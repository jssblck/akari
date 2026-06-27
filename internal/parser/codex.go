package parser

import (
	"time"

	"github.com/tidwall/gjson"
)

// parseCodex parses Codex JSONL. Lines wrap a payload: session_meta carries cwd
// and branch; response_item carries user/assistant turns, function_call (tool
// invocations), function_call_output (tool results), and reasoning; event_msg of
// type token_count carries token usage whose combined input must be split into
// uncached input and cache-read.
func parseCodex(raw []byte) (Session, error) {
	var s Session
	var sp span
	ordinal := 0
	lastAssistant := -1
	callCount := map[int]int{}   // assistant ordinal -> next call index
	toolByID := map[string]int{} // call_id -> index in s.ToolCalls
	currentModel := ""

	// ensureAssistant returns the ordinal of the current assistant turn, hosting a
	// tool call, usage, reasoning, or final text. Codex emits a turn as a run of
	// reasoning and function_call items followed by the assistant message, so all
	// of these fold into one message; a fresh turn begins only after a user item
	// resets lastAssistant.
	ensureAssistant := func(ts time.Time) int {
		if lastAssistant >= 0 {
			return lastAssistant
		}
		s.Messages = append(s.Messages, Message{Ordinal: ordinal, Role: RoleAssistant, Model: currentModel, Timestamp: ts})
		lastAssistant = ordinal
		ordinal++
		return lastAssistant
	}

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
		p := e.Get("payload")
		ts := parseTime(e.Get("timestamp").String())
		sp.observe(ts)
		if m := p.Get("model").String(); m != "" {
			currentModel = m
		}

		switch typ {
		case "session_meta":
			if cwd := p.Get("cwd").String(); cwd != "" {
				s.Cwd = cwd
			}
			if br := p.Get("git.branch").String(); br != "" {
				s.GitBranch = br
			}

		case "response_item":
			switch {
			case p.Get("type").String() == "function_call":
				ord := ensureAssistant(ts)
				s.Messages[ord].HasToolUse = true
				name := p.Get("name").String()
				args := p.Get("arguments").String()
				tc := ToolCall{
					MessageOrdinal: ord, CallIndex: callCount[ord],
					ToolName: name, Category: toolCategory(name),
					InputJSON: args,
				}
				if gjson.Valid(args) {
					tc.FilePath = gjson.Get(args, "file_path").String()
				}
				if cid := p.Get("call_id").String(); cid != "" {
					toolByID[cid] = len(s.ToolCalls)
				}
				s.ToolCalls = append(s.ToolCalls, tc)
				callCount[ord]++

			case p.Get("type").String() == "function_call_output":
				applyToolResult(&s, toolByID, p.Get("call_id").String(), p.Get("output"), false)

			case p.Get("type").String() == "reasoning":
				ord := ensureAssistant(ts)
				if t := blockText(p.Get("content")); t != "" {
					s.Messages[ord].ThinkingText = joinNonEmpty(s.Messages[ord].ThinkingText, t)
					s.Messages[ord].HasThinking = true
				}

			case p.Get("role").String() == "user":
				s.Messages = append(s.Messages, Message{
					Ordinal: ordinal, Role: RoleUser, Content: blockText(p.Get("content")), Timestamp: ts,
				})
				ordinal++
				lastAssistant = -1 // a user turn ends the current assistant turn

			case p.Get("role").String() == "assistant":
				// Fold the final text into the current turn's message, which any
				// preceding reasoning or function_call items already created.
				ord := ensureAssistant(ts)
				if c := blockText(p.Get("content")); c != "" {
					s.Messages[ord].Content = joinNonEmpty(s.Messages[ord].Content, c)
				}
				if currentModel != "" {
					s.Messages[ord].Model = currentModel
				}
			}

		case "event_msg":
			if p.Get("type").String() == "token_count" {
				u := p.Get("info.last_token_usage")
				if !u.Exists() {
					continue
				}
				total := int(u.Get("input_tokens").Int())
				cached := int(u.Get("cached_input_tokens").Int())
				input := total - cached
				if input < 0 {
					input = 0
				}
				usage := Usage{
					Model: currentModel, Input: input, Output: int(u.Get("output_tokens").Int()),
					CacheRead: cached, Reasoning: int(u.Get("reasoning_output_tokens").Int()),
					OccurredAt: ts,
				}
				if lastAssistant >= 0 {
					ord := lastAssistant
					usage.MessageOrdinal = &ord
				}
				s.UsageEvent = append(s.UsageEvent, usage)
			}
		}
	}

	s.StartedAt, s.EndedAt = sp.started, sp.ended
	return s, nil
}

func joinNonEmpty(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\n" + b
}
