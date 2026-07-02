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
				argsVal := p.Get("arguments")
				tc := ToolCall{
					MessageOrdinal: ord, CallIndex: r.openCalls,
					ToolName: name, Category: toolCategory(name),
					CallUID: p.Get("call_id").String(),
				}
				if ref, ok := asCASRef(argsVal); ok {
					tc.InputSHA256, tc.InputBytes, tc.InputMediaType = ref.SHA256, ref.Bytes, ref.MediaType
					tc.FilePath = ref.FilePath
					tc.Detail = ref.Detail
				} else {
					// Codex stores arguments as a JSON-encoded string; the body is the
					// unquoted string value, matching what the client lifts to the CAS.
					args := argsVal.String()
					tc.InputJSON = args
					if gjson.Valid(args) {
						tc.FilePath = gjson.Get(args, "file_path").String()
						tc.Detail = inputDetail(args)
					}
				}
				r.d.ToolCalls = append(r.d.ToolCalls, tc)
				r.openCalls++

			case p.Get("type").String() == "custom_tool_call":
				// A custom tool call (for example apply_patch) carries its input as a
				// plain string, which can be a large patch; record it like any tool call
				// so its body lands in the CAS rather than inline in the transcript.
				ord := r.ensureAssistant(ts)
				r.open.HasToolUse = true
				name := p.Get("name").String()
				tc := ToolCall{
					MessageOrdinal: ord, CallIndex: r.openCalls,
					ToolName: name, Category: toolCategory(name),
					CallUID: p.Get("call_id").String(),
				}
				inVal := p.Get("input")
				if ref, ok := asCASRef(inVal); ok {
					tc.InputSHA256, tc.InputBytes, tc.InputMediaType = ref.SHA256, ref.Bytes, ref.MediaType
				} else {
					tc.InputJSON = inVal.String()
					tc.InputMediaType = "text/plain"
				}
				r.d.ToolCalls = append(r.d.ToolCalls, tc)
				r.openCalls++

			case p.Get("type").String() == "function_call_output",
				p.Get("type").String() == "custom_tool_call_output":
				r.applyResult(p.Get("call_id").String(), p.Get("output"), false)

			case p.Get("type").String() == "image_generation_call":
				// The generated image rides inline as a base64 result; record it as an
				// attachment on the assistant turn (and the client lifts its bytes to the
				// CAS), so the transcript stays small and the image is stored decoded.
				ord := r.ensureAssistant(ts)
				r.open.HasToolUse = true
				r.addAttachment(ord, p.Get("result"), lastPathSegment(p.Get("saved_path").String()))

			case p.Get("type").String() == "reasoning":
				r.ensureAssistant(ts)
				r.addOpenThinking(blockText(p.Get("content")))

			case p.Get("role").String() == "user":
				r.closeTurn() // a user turn ends the current assistant turn
				ord := r.addUser(blockText(p.Get("content")), ts)
				// A user message can paste images as input_image blocks; lift each as an
				// attachment on this message. Non-image blocks are ignored by addAttachment.
				for _, b := range p.Get("content").Array() {
					r.addAttachment(ord, b.Get("image_url"), "")
				}

			case p.Get("role").String() == "assistant":
				r.ensureAssistant(ts)
				r.addOpenContent(blockText(p.Get("content")))
				if r.st.Model != "" {
					r.open.Model = r.st.Model
				}
			}

		case "event_msg":
			switch p.Get("type").String() {
			case "token_count":
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
				if r.open != nil {
					ord := r.open.Ordinal
					usage.MessageOrdinal = &ord
				}
				r.addUsage(usage, offset)

			case "image_generation_end":
				// The streaming completion event mirrors image_generation_call's result;
				// record it as an attachment (deduped against the call by content key) so
				// an image that arrives only as an end event is still stored and referenced.
				r.addAttachment(r.attachOrdinal(ts), p.Get("result"), lastPathSegment(p.Get("saved_path").String()))

			case "user_message":
				// A user_message event carries pasted images as a flat array of data URIs,
				// mirroring the response_item message; record each (deduped by content key).
				ord := r.attachOrdinal(ts)
				for _, img := range p.Get("images").Array() {
					r.addAttachment(ord, img, "")
				}
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
