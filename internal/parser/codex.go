package parser

import (
	"strings"

	"github.com/tidwall/gjson"
)

// reduceCodex advances a Codex session over one raw region. Lines wrap a payload:
// session_meta carries cwd and branch; response_item carries user/assistant
// turns, function_call (tool invocations), function_call_output (tool results),
// and reasoning; event_msg of type token_count carries usage whose combined input
// must be split into uncached input and cache-read. A turn is a run of reasoning
// and function_call items followed by the assistant message, all folded into one
// assistant message; the open turn lives in the reducer, so the fold crosses
// region boundaries freely and Finish flushes the last one.
func (r *reducer) reduceCodex(region []byte, base int64) error {
	return eachLine(region, base, func(line []byte, offset int64) error {
		if !gjson.ValidBytes(line) {
			return nil
		}
		e := gjson.ParseBytes(line)
		typ := e.Get("type").String()
		p := e.Get("payload")
		ts := parseTime(e.Get("timestamp").String())
		r.observe(ts)
		if m := p.Get("model").String(); m != "" {
			r.model = m
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
				r.addCodexReasoning(p)

			case p.Get("role").String() == "user":
				// addUser/addContext close the open assistant turn themselves.
				text := blockText(p.Get("content"))
				r.promoteCodexOpeningContext()
				if isCodexContext(text) {
					// Injected framing is not a human prompt. Recording it as context
					// keeps it out of the title, user count, and prompt hygiene. These
					// turns carry no pasted images.
					r.addContext(text, ts)
					return nil
				}
				ord := r.addUser(text, ts)
				// A user message can paste images as input_image blocks; lift each as an
				// attachment on this message. Non-image blocks are ignored by addAttachment.
				for _, b := range p.Get("content").Array() {
					r.addAttachment(ord, b.Get("image_url"), "")
				}

			case p.Get("role").String() == "assistant":
				r.ensureAssistant(ts)
				r.addOpenContent(blockText(p.Get("content")))
				if r.model != "" {
					r.open.Model = r.model
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
					Model: r.model, Input: input, Output: int(u.Get("output_tokens").Int()),
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
}

// promoteCodexOpeningContext handles injected framing without depending on its
// contents. Codex writes framing and the human prompt as consecutive opening user
// turns. When the second arrives, the first is the Context section. An open
// assistant turn proves the user turns were separated by a response, so it blocks
// the promotion. Marker matching remains responsible for framing later in a session.
func (r *reducer) promoteCodexOpeningContext() {
	if r.open != nil || len(r.d.Messages) != 1 {
		return
	}
	first := &r.d.Messages[0]
	if first.Ordinal == 0 && first.Role == RoleUser {
		first.Role = RoleContext
	}
}

var codexContextMarkers = [...]string{
	"# AGENTS.md instructions for ",
	"<environment_context>",
	"<user_instructions>",
	"<recommended_plugins>",
}

// isCodexContext reports whether a Codex user turn contains at least two distinct
// framing markers. The markers can occur in any order because Codex adds new injected
// blocks over time. Requiring two avoids treating a human prompt that quotes one marker
// as context. Opening framing also has a structural fallback in
// promoteCodexOpeningContext.
func isCodexContext(text string) bool {
	found := 0
	for _, marker := range codexContextMarkers {
		if !strings.Contains(text, marker) {
			continue
		}
		found++
		if found == 2 {
			return true
		}
	}
	return false
}

// addCodexReasoning records one Codex reasoning item on the open turn. Codex ships
// the reasoning either in the clear (older sessions: a "content" block, or a
// "summary" of text blocks) or, in current builds, as an opaque "encrypted_content"
// blob with the summary and content dropped. The weight sums whatever is present, so
// an encrypted item still records its reasoning volume through the ciphertext length
// (which tracks the reasoning-token count at r=0.997); ThinkingText carries whatever
// plaintext survived.
func (r *reducer) addCodexReasoning(p gjson.Result) {
	text := joinNonEmpty(blockText(p.Get("content")), summaryText(p.Get("summary")))
	weight := len(text) + len(p.Get("encrypted_content").String())
	r.addOpenReasoning(text, weight)
}

// summaryText flattens a Codex reasoning "summary" (an array of summary_text blocks)
// to its text. It is separate from blockText because a summary block's type is
// "summary_text", which blockText's text-block set does not include.
func summaryText(v gjson.Result) string {
	if !v.IsArray() {
		return ""
	}
	var parts []string
	for _, b := range v.Array() {
		if t := b.Get("text").String(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
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
