package parser

import "github.com/tidwall/gjson"

// reduceCodex advances a Codex session over one raw region. Lines wrap a payload:
// session_meta carries cwd and branch; response_item carries user/assistant
// turns, function_call (tool invocations), function_call_output (tool results),
// and reasoning; event_msg of type token_count carries usage whose combined input
// must be split into uncached input and cache-read. A turn is a run of reasoning
// and function_call items followed by the assistant message, all folded into one
// assistant message; that fold can span a chunk boundary, which is why the open
// turn lives in the carry-over state.
func (r *reducer) reduceCodex(region []byte, base int64) error {
	err := eachLine(region, base, func(line []byte, offset int64) error {
		if !gjson.ValidBytes(line) {
			return nil
		}
		e := gjson.ParseBytes(line)
		typ := e.Get("type").String()
		p := e.Get("payload")
		ts := parseTime(e.Get("timestamp").String())
		r.observe(ts)
		if m := p.Get("model").String(); m != "" {
			r.st.Model = m
		}

		switch typ {
		case "session_meta":
			if cwd := p.Get("cwd").String(); cwd != "" {
				r.d.Cwd = cwd
			}
			if br := p.Get("git.branch").String(); br != "" {
				r.d.GitBranch = br
			}

		case "response_item":
			switch {
			case p.Get("type").String() == "function_call":
				ord := r.ensureAssistant(ts)
				r.open.HasToolUse = true
				name := p.Get("name").String()
				args := p.Get("arguments").String()
				tc := ToolCall{
					MessageOrdinal: ord, CallIndex: r.st.OpenAssistantCalls,
					ToolName: name, Category: toolCategory(name),
					InputJSON: args, CallUID: p.Get("call_id").String(), SourceOffset: offset,
				}
				if gjson.Valid(args) {
					tc.FilePath = gjson.Get(args, "file_path").String()
				}
				r.d.ToolCalls = append(r.d.ToolCalls, tc)
				r.st.OpenAssistantCalls++

			case p.Get("type").String() == "function_call_output":
				r.applyResult(p.Get("call_id").String(), p.Get("output"), false)

			case p.Get("type").String() == "reasoning":
				r.ensureAssistant(ts)
				if t := blockText(p.Get("content")); t != "" {
					r.open.AppendThinking = joinNonEmpty(r.open.AppendThinking, t)
					r.open.HasThinking = true
				}

			case p.Get("role").String() == "user":
				r.closeTurn() // a user turn ends the current assistant turn
				r.addUser(blockText(p.Get("content")), ts)

			case p.Get("role").String() == "assistant":
				r.ensureAssistant(ts)
				if c := blockText(p.Get("content")); c != "" {
					r.open.AppendContent = joinNonEmpty(r.open.AppendContent, c)
				}
				if r.st.Model != "" {
					r.open.Model = r.st.Model
				}
			}

		case "event_msg":
			if p.Get("type").String() == "token_count" {
				u := p.Get("info.last_token_usage")
				if !u.Exists() {
					return nil
				}
				total := int(u.Get("input_tokens").Int())
				cached := int(u.Get("cached_input_tokens").Int())
				input := total - cached
				if input < 0 {
					input = 0
				}
				usage := Usage{
					Model: r.st.Model, Input: input, Output: int(u.Get("output_tokens").Int()),
					CacheRead: cached, Reasoning: int(u.Get("reasoning_output_tokens").Int()),
					OccurredAt: ts,
				}
				if r.st.OpenAssistant >= 0 {
					ord := r.st.OpenAssistant
					usage.MessageOrdinal = &ord
				}
				r.addUsage(usage, offset)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// Keep any still-open turn open so the next region continues its row.
	r.flushRegion()
	return nil
}

func joinNonEmpty(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "\n" + b
}
