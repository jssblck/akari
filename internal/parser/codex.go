package parser

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

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
			// The full system prompt is logged verbatim; a resumed session rewrites
			// the meta line, so an unchanged prompt is not re-emitted.
			instructions := p.Get("base_instructions.text").String()
			if instructions == "" {
				instructions = p.Get("base_instructions").String()
			}
			if instructions != "" && instructions != r.lastSystemText {
				r.lastSystemText = instructions
				r.addSystem(instructions, ts)
			}
			// A subagent thread declares its parent and its role inline.
			if parent := p.Get("parent_thread_id").String(); parent != "" {
				r.d.Identity.ParentSourceID = parent
			}
			if name := lastPathSegment(p.Get("agent_path").String()); name != "" && name != "root" {
				r.d.Identity.SubagentName = name
			} else if nick := p.Get("agent_nickname").String(); nick != "" {
				r.d.Identity.SubagentName = nick
			}

		case "turn_context":
			// Per-turn settings. The developer instructions repeat identically on
			// every turn of one mode, so only a change emits a context turn.
			settings := p.Get("collaboration_mode.settings")
			if effort := settings.Get("reasoning_effort").String(); effort != "" {
				r.d.Identity.ReasoningEffort = effort
			}
			if sandbox := p.Get("sandbox_policy.type").String(); sandbox != "" {
				r.d.Identity.PermissionMode = sandbox
			}
			if dev := settings.Get("developer_instructions").String(); dev != "" && dev != r.lastDevInstructions {
				r.lastDevInstructions = dev
				r.addContext(dev, ts)
			}

		case "world_state":
			// The rendered AGENTS.md the agent actually saw; snapshots repeat, so
			// only a change emits a context turn.
			if md := p.Get("state.agents_md.text").String(); md != "" && md != r.lastAgentsMD {
				r.lastAgentsMD = md
				r.addContext(md, ts)
			}

		case "compacted":
			r.addEvent(EventCompaction, nil, ts)

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
				r.recordCall(tc)

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
				r.recordCall(tc)

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

			case p.Get("type").String() == "agent_message":
				// Inter-agent mail (a subagent's report to its parent, or vice versa).
				// It is input the harness injected, not a human prompt, so it takes the
				// context role; the payload text already names its sender and type. It
				// arrives mid-turn, so it must not close the fold.
				if text := blockText(p.Get("content")); text != "" {
					r.addContextInline(text, ts)
				}

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
				// The combined input splits three ways: cache reads, cache writes, and
				// the uncached remainder. Codex reports writes as their own count (zero
				// on every current OpenAI rollout, but the field is live in the schema),
				// so it must come out of the combined figure like the reads do or a
				// future nonzero write would double-bill.
				total := int(u.Get("input_tokens").Int())
				cached := int(u.Get("cached_input_tokens").Int())
				written := int(u.Get("cache_write_input_tokens").Int())
				input := total - cached - written
				if input < 0 {
					input = 0
				}
				usage := Usage{
					Model: r.model, Input: input, Output: int(u.Get("output_tokens").Int()),
					CacheWrite: written, CacheRead: cached,
					Reasoning:  int(u.Get("reasoning_output_tokens").Int()),
					OccurredAt: ts,
				}
				if r.open != nil {
					ord := r.open.Ordinal
					usage.MessageOrdinal = &ord
				}
				r.addUsage(usage, offset)

			case "agent_reasoning":
				// The streamed reasoning summary. Current builds encrypt the reasoning
				// items and drop their summaries, so this event text is usually the only
				// surviving plaintext; buildOpen keeps it only when the items carried
				// none of their own.
				if text := p.Get("text").String(); text != "" {
					r.ensureAssistant(ts)
					r.openThinkEvents = append(r.openThinkEvents, text)
				}

			case "task_complete":
				r.addEvent(EventTurnEnd, map[string]any{
					"duration_ms": p.Get("duration_ms").Int(),
					"ttft_ms":     p.Get("time_to_first_token_ms").Int(),
				}, ts)

			case "turn_aborted":
				r.addEvent(EventTurnAborted, map[string]any{
					"reason":      p.Get("reason").String(),
					"duration_ms": p.Get("duration_ms").Int(),
				}, ts)

			case "sub_agent_activity":
				r.addEvent(EventSubagentActivity, map[string]any{
					"thread_id":  p.Get("agent_thread_id").String(),
					"agent_path": p.Get("agent_path").String(),
					"state":      p.Get("kind").String(),
				}, ts)

			case "web_search_end":
				// Web searches exist only as events (no response item), so the event is
				// the call: query and action as the input, the result list as the body.
				input := codexEventInputJSON(map[string]gjson.Result{
					"query":  p.Get("query"),
					"action": p.Get("action"),
				})
				r.codexEventCall(p.Get("call_id").String(), "web_search", ts, input,
					p.Get("results"), false)

			case "mcp_tool_call_end":
				// An MCP call normally also logs a function_call item, which already
				// recorded it; only an end event with no item behind it (an id the
				// parse never saw) creates the call, so nothing lands twice.
				inv := p.Get("invocation")
				name := "mcp__" + inv.Get("server").String() + "__" + inv.Get("tool").String()
				r.codexEventCall(p.Get("call_id").String(), name, ts,
					inv.Get("arguments").Raw, p.Get("result"), p.Get("result.Err").Exists())

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
// the promotion, and any earlier real turn does too; the system prompt and other
// injected turns that now precede the opening pair are transparent to the rule.
// Marker matching remains responsible for framing later in a session.
func (r *reducer) promoteCodexOpeningContext() {
	if r.open != nil || len(r.d.Messages) == 0 {
		return
	}
	last := &r.d.Messages[len(r.d.Messages)-1]
	if last.Role != RoleUser {
		return
	}
	for _, m := range r.d.Messages[:len(r.d.Messages)-1] {
		if m.Role != RoleSystem && m.Role != RoleContext {
			return
		}
	}
	last.Role = RoleContext
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

// recordCall appends a tool call to the delta and remembers its id, so an end
// event that mirrors an already-recorded call can be told from one that is the
// only trace of its call.
func (r *reducer) recordCall(tc ToolCall) {
	r.d.ToolCalls = append(r.d.ToolCalls, tc)
	r.openCalls++
	if tc.CallUID != "" {
		if r.seenCalls == nil {
			r.seenCalls = map[string]bool{}
		}
		r.seenCalls[tc.CallUID] = true
	}
}

// codexEventCall records a tool call known only from its completion event: the
// event supplies the input and the result together, so the call and its result
// op are emitted in one step. An id already recorded from a response item means
// the call is already in the delta and the event adds nothing.
func (r *reducer) codexEventCall(callID, name string, ts time.Time, inputJSON string, result gjson.Result, isErr bool) {
	if callID == "" || r.seenCalls[callID] {
		return
	}
	ord := r.ensureAssistant(ts)
	r.open.HasToolUse = true
	tc := ToolCall{
		MessageOrdinal: ord, CallIndex: r.openCalls,
		ToolName: name, Category: toolCategory(name),
		CallUID:   callID,
		InputJSON: inputJSON,
	}
	if gjson.Valid(inputJSON) {
		tc.Detail = inputDetail(inputJSON)
	}
	r.recordCall(tc)
	if result.Exists() {
		status := "ok"
		if isErr {
			status = "error"
		}
		body := result.Raw
		if result.Type == gjson.String {
			body = result.String()
		}
		media := "application/json"
		if result.Type == gjson.String {
			media = "text/plain"
		}
		r.d.ToolResults = append(r.d.ToolResults, ToolResultOp{
			CallUID: callID, Status: status,
			Body: body, Bytes: len(body), MediaType: media,
		})
	}
}

// codexEventInputJSON assembles a synthetic input body from named payload
// fields, skipping the absent ones. Fields keep their raw JSON form; the fixed
// alphabetical key order keeps the assembled body deterministic.
func codexEventInputJSON(fields map[string]gjson.Result) string {
	keys := make([]string, 0, len(fields))
	for k, v := range fields {
		if v.Exists() {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.WriteString(fields[k].Raw)
	}
	b.WriteByte('}')
	return b.String()
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
