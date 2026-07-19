package parser

import (
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// reduceClaude advances a Claude Code session over one raw region. Each line is a
// typed entry; user and assistant entries carry content as a string or an array
// of typed blocks (text, thinking, tool_use, tool_result). Token usage rides on
// the assistant entry. Tool results arrive as blocks in a following user entry
// and are back-patched to their tool_use by id.
//
// Claude Code logs each content block of one API assistant response on its own
// JSONL line (the thinking, the text, and every tool_use arrive as separate
// assistant entries sharing the response's message.id), so assistant lines with
// one id fold into a single turn: one messages row per API response, not per
// content block (issue #98). Tool-result-only user lines are transparent to the
// fold, because a response with parallel tool calls logs each call's result
// between its own tool_use lines. What ends a turn is a real user message or an
// assistant line with a different (or missing) id, so a resumed or compacted
// transcript that replays an old id later still produces its own row, exactly
// as the replayed tool calls do.
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
		if slug := e.Get("slug").String(); slug != "" {
			r.d.Identity.Slug = slug
		}
		// Every line of a subagent transcript stamps the agent type that ran it;
		// that is the session's role for its parent, not a per-line fact.
		if e.Get("isSidechain").Bool() {
			if agent := e.Get("attributionAgent").String(); agent != "" {
				r.d.Identity.SubagentName = agent
			}
		}

		switch typ {
		case "user":
			content := e.Get("message.content")
			text := blockText(content)
			hasImage := false
			if content.IsArray() {
				var resultIDs []string
				for _, b := range content.Array() {
					switch b.Get("type").String() {
					case "tool_result":
						id := b.Get("tool_use_id").String()
						resultIDs = append(resultIDs, id)
						r.applyResult(id, b.Get("content"), b.Get("is_error").Bool())
					case "image":
						hasImage = true
					}
				}
				// The top-level toolUseResult is the same result in structured form. It
				// is unkeyed, so it can only be matched when the line delivered exactly
				// one result; a multi-result line leaves it ambiguous and dropped.
				if len(resultIDs) == 1 {
					r.applyStructResult(resultIDs[0], e.Get("toolUseResult"))
				}
			}
			if strings.TrimSpace(text) == "" && !hasImage {
				// A turn that only delivers tool results is not a message, and it does
				// not close the open assistant turn either: when one response carries
				// parallel tool calls, Claude Code logs each call's result between that
				// response's own content-block lines, so the response's remaining
				// tool_use lines (same message.id) are still part of the open turn. A
				// real user message or a different id is what ends the response. An
				// image-only paste is a message even with no text.
				return nil
			}
			// isMeta marks injected material (a skill body, a coordinator note, an
			// image caption); isCompactSummary marks the replayed summary after a
			// compaction. Neither is a human prompt, so both take the context role and
			// stay out of the title, user count, and prompt hygiene.
			var ord int
			if e.Get("isMeta").Bool() || e.Get("isCompactSummary").Bool() {
				ord = r.addContext(text, ts)
			} else {
				ord = r.addUser(text, ts)
			}
			// Pasted images arrive as typed image blocks carrying base64 at
			// source.data (a CAS sentinel once the client lifted it); record each as
			// an attachment on this message.
			if hasImage {
				for _, b := range content.Array() {
					if b.Get("type").String() == "image" {
						r.addAttachment(ord, b.Get("source.data"), "")
					}
				}
			}

		case "assistant":
			msg := e.Get("message")
			// Fold assistant lines sharing one API message id into one turn. A
			// different id (or none) closes the open turn and starts fresh; a real
			// user message in between closes it too (addUser does that itself).
			id := msg.Get("id").String()
			if r.open == nil || id == "" || id != r.openClaudeID {
				r.closeTurn()
			}
			ord := r.ensureAssistant(ts)
			r.openClaudeID = id
			if m := msg.Get("model").String(); m != "" {
				r.open.Model = m
			}
			for _, b := range msg.Get("content").Array() {
				switch b.Get("type").String() {
				case "text":
					r.addOpenContent(b.Get("text").String())
				case "thinking":
					// The signature is the encrypted thinking Claude ships in place of the
					// redacted plaintext; its length tracks the hidden reasoning volume
					// (r=0.97 against blocks that kept their text), so it is the weight when
					// the text is gone and rides alongside it when kept.
					t := b.Get("thinking").String()
					r.addOpenReasoning(t, len(t)+len(b.Get("signature").String()))
				case "redacted_thinking":
					// A fully redacted block carries only an opaque "data" blob and no text;
					// its length is the reasoning weight.
					r.addOpenReasoning("", len(b.Get("data").String()))
				case "tool_use":
					r.open.HasToolUse = true
					name := b.Get("name").String()
					tc := ToolCall{
						MessageOrdinal: ord, CallIndex: r.openCalls,
						ToolName: name, Category: toolCategory(name),
						FilePath: b.Get("input.file_path").String(),
						CallUID:  b.Get("id").String(),
						// The line-level attribution stamps what drove this call: the
						// subagent type, the invoked skill, and its plugin. They co-occur
						// freely, so all three are carried.
						AttributionAgent:  e.Get("attributionAgent").String(),
						AttributionSkill:  e.Get("attributionSkill").String(),
						AttributionPlugin: e.Get("attributionPlugin").String(),
					}
					setToolInput(&tc, b.Get("input"), "application/json")
					r.d.ToolCalls = append(r.d.ToolCalls, tc)
					r.openCalls++
				}
			}

			// Every line of the response repeats the same usage block; each is emitted
			// and the rebuild's dedup keeps one per DedupKey (the API message id).
			if u := msg.Get("usage"); u.Exists() {
				o := ord
				r.addUsage(Usage{
					MessageOrdinal: &o,
					Model:          msg.Get("model").String(),
					Input:          int(u.Get("input_tokens").Int()),
					Output:         int(u.Get("output_tokens").Int()),
					CacheWrite:     int(u.Get("cache_creation_input_tokens").Int()),
					CacheRead:      int(u.Get("cache_read_input_tokens").Int()),
					OccurredAt:     ts,
					DedupKey:       msg.Get("id").String(),
				}, offset)
			}

			r.claudeFallbackFromAssistant(e, msg, ord, ts)

		case "system":
			switch e.Get("subtype").String() {
			case "model_refusal_fallback":
				// The safety-classifier fallback notice arrives on its own line, separate
				// from the assistant entries, and carries the refusal detail the
				// assistant side does not: trigger, category, and explanation.
				r.claudeFallbackFromSystem(e, ts)
			case "compact_boundary":
				m := e.Get("compactMetadata")
				r.addEvent(EventCompaction, map[string]any{
					"trigger":        m.Get("trigger").String(),
					"pre_tokens":     m.Get("preTokens").Int(),
					"post_tokens":    m.Get("postTokens").Int(),
					"dropped_tokens": m.Get("cumulativeDroppedTokens").Int(),
				}, ts)
			case "turn_duration":
				r.addEvent(EventTurnEnd, map[string]any{
					"duration_ms":   e.Get("durationMs").Int(),
					"message_count": e.Get("messageCount").Int(),
				}, ts)
			case "api_error":
				msg := e.Get("error.formatted").String()
				if msg == "" {
					msg = e.Get("error.message").String()
				}
				r.addEvent(EventAPIError, map[string]any{
					"message":       msg,
					"retry_attempt": e.Get("retryAttempt").Int(),
					"max_retries":   e.Get("maxRetries").Int(),
				}, ts)
			case "stop_hook_summary":
				attrs := map[string]any{
					"hook_count":             e.Get("hookCount").Int(),
					"prevented_continuation": e.Get("preventedContinuation").Bool(),
				}
				if reason := e.Get("stopReason").String(); reason != "" {
					attrs["stop_reason"] = reason
				}
				if errs := e.Get("hookErrors"); errs.IsArray() && len(errs.Array()) > 0 {
					var msgs []string
					for _, he := range errs.Array() {
						msgs = append(msgs, he.String())
					}
					attrs["errors"] = msgs
				}
				r.addEvent(EventStopHook, attrs, ts)
			}

		case "attachment":
			// Attachment entries carry the material Claude Code injects between turns.
			// Only the instruction-bearing kinds become context turns; the per-turn
			// state reminders (task lists, queued commands, file snapshots, hook
			// output) stay dropped as noise.
			if text, ok := claudeAttachmentContext(e.Get("attachment")); ok {
				r.addContext(text, ts)
			}

		case "custom-title":
			if t := e.Get("customTitle").String(); t != "" {
				r.d.Identity.CustomTitle = t
			}

		case "permission-mode":
			if m := e.Get("permissionMode").String(); m != "" {
				r.d.Identity.PermissionMode = m
			}

		case "pr-link":
			r.d.Identity.PRNumber = int(e.Get("prNumber").Int())
			r.d.Identity.PRURL = e.Get("prUrl").String()
			r.d.Identity.PRRepo = e.Get("prRepository").String()
		}
		return nil
	})
}

// claudeAttachmentContext renders an instruction-bearing attachment entry as
// context text, or reports false for the kinds that are per-turn state noise.
// Each renderer keeps the payload's own prose and drops the envelope.
func claudeAttachmentContext(a gjson.Result) (string, bool) {
	switch a.Get("type").String() {
	case "skill_listing":
		if c := a.Get("content").String(); c != "" {
			return "Available skills:\n" + c, true
		}
	case "mcp_instructions_delta":
		if blocks := joinStrings(a.Get("addedBlocks"), "\n\n"); blocks != "" {
			return "MCP server instructions:\n\n" + blocks, true
		}
	case "agent_listing_delta":
		if lines := joinStrings(a.Get("addedLines"), "\n"); lines != "" {
			return "Available agents:\n" + lines, true
		}
	case "deferred_tools_delta":
		if names := joinStrings(a.Get("addedNames"), ", "); names != "" {
			return "Deferred tools available: " + names, true
		}
	case "nested_memory":
		if c := a.Get("content.content").String(); c != "" {
			if path := a.Get("path").String(); path != "" {
				return "Memory from " + path + ":\n\n" + c, true
			}
			return c, true
		}
	case "command_permissions":
		if tools := joinStrings(a.Get("allowedTools"), ", "); tools != "" {
			return "Allowed tools: " + tools, true
		}
	case "invoked_skills":
		var parts []string
		for _, s := range a.Get("skills").Array() {
			if c := s.Get("content").String(); c != "" {
				parts = append(parts, c)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n\n"), true
		}
	}
	return "", false
}

// joinStrings joins a JSON array of strings, skipping empties.
func joinStrings(arr gjson.Result, sep string) string {
	if !arr.IsArray() {
		return ""
	}
	var parts []string
	for _, v := range arr.Array() {
		if s := v.String(); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, sep)
}

// claudeFallbackFromAssistant emits a FallbackOp when the assistant entry carries an
// explicit fallback marker: a "fallback" content block OR a usage.iterations entry of
// type "fallback_message". It keys ONLY on these markers, never on the model string
// changing, so an intentional switch is not flagged. A normal turn also carries
// usage.iterations (a single type=="message" entry, whose model may be absent), so the
// presence of iterations alone is not a fallback: only a "fallback_message" entry is.
//
// From the block it reads from.model and to.model; from iterations it reads ToModel
// from the fallback_message entry (else the message model) and FromModel from the last
// type=="message" entry, and sums that entry's token counts as the declined attempt's
// spend. A sticky-routed turn carries a fallback_message iteration entry with no block,
// so the two sources are checked independently. The DedupKey is the top-level requestId
// when present, else the message id, so every chunk of one API message merges to one row.
func (r *reducer) claudeFallbackFromAssistant(e, msg gjson.Result, ord int, ts time.Time) {
	block := msg.Get(`content.#(type=="fallback")`)
	iterations := msg.Get("usage.iterations")

	var fallbackIter, lastMessageIter gjson.Result
	var haveFallbackIter, haveMessageIter bool
	if iterations.IsArray() {
		for _, it := range iterations.Array() {
			switch it.Get("type").String() {
			case "fallback_message":
				fallbackIter = it
				haveFallbackIter = true
			case "message":
				lastMessageIter = it
				haveMessageIter = true
			}
		}
	}

	if !block.Exists() && !haveFallbackIter {
		return // no explicit marker: an ordinary turn, not a fallback
	}

	op := FallbackOp{OccurredAt: ts, DedupKey: claudeDedupKey(e, msg)}
	o := ord
	op.MessageOrdinal = &o

	switch {
	case block.Exists():
		op.FromModel = block.Get("from.model").String()
		op.ToModel = block.Get("to.model").String()
	default:
		if haveMessageIter {
			op.FromModel = lastMessageIter.Get("model").String()
		}
	}
	if op.ToModel == "" {
		if haveFallbackIter {
			op.ToModel = fallbackIter.Get("model").String()
		}
		if op.ToModel == "" {
			op.ToModel = msg.Get("model").String()
		}
	}

	// The declined attempt's spend is the sum over the type=="message" iteration entries.
	// It is only meaningful when a fallback_message entry is present (an ordinary turn's
	// lone message entry is the served turn, not a declined one), so it is summed only then.
	if haveFallbackIter && iterations.IsArray() {
		op.DeclinedObserved = true
		for _, it := range iterations.Array() {
			if it.Get("type").String() != "message" {
				continue
			}
			op.DeclinedInput += int(it.Get("input_tokens").Int())
			op.DeclinedOutput += int(it.Get("output_tokens").Int())
			op.DeclinedCacheWrite += int(it.Get("cache_creation_input_tokens").Int())
			op.DeclinedCacheRead += int(it.Get("cache_read_input_tokens").Int())
		}
	}

	r.d.Fallbacks = append(r.d.Fallbacks, op)
}

// claudeFallbackFromSystem emits a FallbackOp from a model_refusal_fallback system entry.
// It carries the refusal detail the assistant side lacks (trigger, category, explanation)
// and shares the assistant entry's requestId as its DedupKey, so the store merges the two
// into one row. It produces no message row and no MessageOrdinal, so it never disturbs the
// message ordinal sequence.
func (r *reducer) claudeFallbackFromSystem(e gjson.Result, ts time.Time) {
	r.d.Fallbacks = append(r.d.Fallbacks, FallbackOp{
		FromModel:          e.Get("originalModel").String(),
		ToModel:            e.Get("fallbackModel").String(),
		Trigger:            e.Get("trigger").String(),
		RefusalCategory:    e.Get("apiRefusalCategory").String(),
		RefusalExplanation: e.Get("apiRefusalExplanation").String(),
		OccurredAt:         ts,
		DedupKey:           e.Get("requestId").String(),
	})
}

// claudeDedupKey is the identity that ties every JSONL line of one logical fallback
// together: the top-level requestId when present, else the assistant message id. Claude
// splits one API message across several assistant entries sharing both, and the system
// entry shares the requestId, so all lines of one fallback merge to a single stored row.
func claudeDedupKey(e, msg gjson.Result) string {
	if req := e.Get("requestId").String(); req != "" {
		return req
	}
	return msg.Get("id").String()
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

// applyStructResult amends the result op applyResult just emitted with the
// line's structured toolUseResult. It patches the last op in place (the caller
// guarantees it was appended by the same line for the same call id) rather than
// emitting a second op, because the store's result merge is first-wins per call
// id and a struct-only op would lose. A scalar toolUseResult other than a string
// carries nothing worth storing and is dropped.
func (r *reducer) applyStructResult(id string, v gjson.Result) {
	if id == "" || len(r.d.ToolResults) == 0 {
		return
	}
	op := &r.d.ToolResults[len(r.d.ToolResults)-1]
	if op.CallUID != id {
		return
	}
	if ref, ok := asCASRef(v); ok {
		op.StructSHA256, op.StructBytes, op.StructMediaType = ref.SHA256, ref.Bytes, ref.MediaType
		return
	}
	switch {
	case v.IsObject(), v.IsArray():
		op.StructBody, op.StructBytes, op.StructMediaType = v.Raw, len(v.Raw), "application/json"
	case v.Type == gjson.String:
		s := v.String()
		op.StructBody, op.StructBytes, op.StructMediaType = s, len(s), "text/plain"
	}
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
