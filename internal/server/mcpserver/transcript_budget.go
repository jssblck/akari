package mcpserver

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jssblck/akari/internal/server/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func loadTranscript(ctx context.Context, st *store.Store, sessionID int64, after *int, limit, total, responseBudget int) (*transcriptDTO, error) {
	if limit <= 0 {
		limit = defaultTranscriptLimit
	}
	if limit > maxTranscriptWindow {
		limit = maxTranscriptWindow
	}
	previewChars := responseBudget / 64
	if previewChars < 16 {
		previewChars = 16
	}
	if previewChars > 1024 {
		previewChars = 1024
	}
	// Reserve space for the session header, tool metadata, text summary, and JSON
	// object keys. Exact encoding is checked after those pieces are assembled.
	messageBudget := int64(responseBudget - 64*1024)
	if messageBudget < 1 {
		messageBudget = 1
	}
	msgs, hasMore, byteTruncated, err := st.MCPMessagesAfter(ctx, sessionID, after, limit, messageBudget, previewChars)
	if err != nil {
		return nil, err
	}
	tr := &transcriptDTO{
		Limit: limit, Returned: len(msgs), TotalMessages: total,
		HasMore: hasMore, ByteBudgetTruncated: byteTruncated,
		Messages: make([]messageDTO, 0, len(msgs)), ToolCalls: []toolCallDTO{}, Attachments: []attachmentDTO{},
	}
	for _, m := range msgs {
		dto := messageToDTO(m.Message)
		if m.ContentTruncated {
			uri := messageResourceURI(sessionID, m.Ordinal, "content", m.ContentSHA256)
			n := m.ContentBytes
			dto.ContentByteLen = &n
			dto.ContentReference = &contentReferenceDTO{URI: uri, MediaType: "text/plain; charset=utf-8", ByteLen: n}
		}
		if m.ThinkingTextTruncated {
			uri := messageResourceURI(sessionID, m.Ordinal, "thinking", m.ThinkingTextSHA256)
			n := m.ThinkingTextBytes
			dto.ThinkingTextByteLen = &n
			dto.ThinkingTextReference = &contentReferenceDTO{URI: uri, MediaType: "text/plain; charset=utf-8", ByteLen: n}
		}
		tr.Messages = append(tr.Messages, dto)
	}
	if len(msgs) == 0 {
		return tr, nil
	}
	minOrd, maxOrd := msgs[0].Ordinal, msgs[len(msgs)-1].Ordinal
	if hasMore {
		next := maxOrd
		tr.NextAfter = &next
	}
	calls, err := st.ToolCallsInRange(ctx, sessionID, minOrd, maxOrd)
	if err != nil {
		return nil, err
	}
	for _, c := range calls {
		tr.ToolCalls = append(tr.ToolCalls, toolCallToDTO(c))
	}
	atts, err := st.AttachmentsInRange(ctx, sessionID, minOrd, maxOrd)
	if err != nil {
		return nil, err
	}
	for _, a := range atts {
		tr.Attachments = append(tr.Attachments, attachmentToDTO(a))
	}
	return tr, nil
}

func fitSessionToBudget(r responder, out sessionDetailDTO) (sessionDetailDTO, []*mcp.ResourceLink) {
	if out.Transcript == nil {
		return out, nil
	}
	for {
		links := transcriptLinks(out.Transcript)
		res := &mcp.CallToolResult{Content: sessionContent(out, links)}
		size, err := r.encodedSize(res, out)
		if err == nil && size <= r.budget {
			return out, links
		}
		tr := out.Transcript
		if len(tr.Messages) > 1 {
			tr.Messages = tr.Messages[:len(tr.Messages)-1]
			maxOrdinal := tr.Messages[len(tr.Messages)-1].Ordinal
			tr.ToolCalls = filterToolCalls(tr.ToolCalls, maxOrdinal)
			tr.Attachments = filterAttachments(tr.Attachments, maxOrdinal)
			tr.Returned = len(tr.Messages)
			tr.HasMore = true
			tr.ByteBudgetTruncated = true
			tr.NextAfter = &maxOrdinal
			continue
		}
		if len(tr.Messages) == 1 && externalizeMessage(sessionIDFromDetail(out), &tr.Messages[0]) {
			tr.ByteBudgetTruncated = true
			continue
		}
		return out, links
	}
}

func externalizeMessage(sessionID int64, msg *messageDTO) bool {
	changed := false
	if msg.Content != "" {
		if msg.ContentReference == nil {
			n := int64(len([]byte(msg.Content)))
			msg.ContentByteLen = &n
			msg.ContentReference = &contentReferenceDTO{
				URI: messageResourceURI(sessionID, msg.Ordinal, "content", textSHA256(msg.Content)), MediaType: "text/plain; charset=utf-8", ByteLen: n,
			}
			changed = true
		}
		preview := utf8Prefix(msg.Content, 256)
		changed = changed || preview != msg.Content
		msg.Content = preview
	}
	if msg.ThinkingText != "" {
		if msg.ThinkingTextReference == nil {
			n := int64(len([]byte(msg.ThinkingText)))
			msg.ThinkingTextByteLen = &n
			msg.ThinkingTextReference = &contentReferenceDTO{
				URI: messageResourceURI(sessionID, msg.Ordinal, "thinking", textSHA256(msg.ThinkingText)), MediaType: "text/plain; charset=utf-8", ByteLen: n,
			}
			changed = true
		}
		preview := utf8Prefix(msg.ThinkingText, 256)
		changed = changed || preview != msg.ThinkingText
		msg.ThinkingText = preview
	}
	return changed
}

func textSHA256(s string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

func utf8Prefix(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	b := []byte(s[:maxBytes])
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}

func transcriptLinks(tr *transcriptDTO) []*mcp.ResourceLink {
	var links []*mcp.ResourceLink
	for _, msg := range tr.Messages {
		refs := []struct {
			field string
			ref   *contentReferenceDTO
		}{{"content", msg.ContentReference}, {"thinking", msg.ThinkingTextReference}}
		for _, item := range refs {
			field, ref := item.field, item.ref
			if ref == nil {
				continue
			}
			n := ref.ByteLen
			links = append(links, &mcp.ResourceLink{
				URI: ref.URI, Name: fmt.Sprintf("message-%d-%s", msg.Ordinal, field),
				Description: "Full message field omitted from the byte-bounded transcript page.",
				MIMEType:    ref.MediaType, Size: &n,
			})
		}
	}
	return links
}

func filterToolCalls(in []toolCallDTO, maxOrdinal int) []toolCallDTO {
	for i, call := range in {
		if call.MessageOrdinal > maxOrdinal {
			return in[:i]
		}
	}
	return in
}

func filterAttachments(in []attachmentDTO, maxOrdinal int) []attachmentDTO {
	for i, attachment := range in {
		if attachment.MessageOrdinal > maxOrdinal {
			return in[:i]
		}
	}
	return in
}

func sessionIDFromDetail(out sessionDetailDTO) int64 { return out.ID }

func sessionSummary(out sessionDetailDTO) string {
	if out.Transcript == nil {
		return fmt.Sprintf("get_session: session %d loaded without transcript. Full data is in structuredContent.", out.ID)
	}
	tr := out.Transcript
	if tr.HasMore {
		return fmt.Sprintf("get_session: session %d; %d transcript messages returned; has_more=true; next_after=%d. Full data is in structuredContent.", out.ID, tr.Returned, *tr.NextAfter)
	}
	return fmt.Sprintf("get_session: session %d; %d transcript messages returned; has_more=false. Full data is in structuredContent.", out.ID, tr.Returned)
}

func sessionContent(out sessionDetailDTO, links []*mcp.ResourceLink) []mcp.Content {
	content := []mcp.Content{&mcp.TextContent{Text: sessionSummary(out)}}
	for _, link := range links {
		content = append(content, link)
	}
	return content
}

// fitSessionsToBudget trims rows off the end of a list_sessions page until the
// encoded result fits r.budget. Trimming stops at one row rather than zero: a
// row that alone exceeds the budget (an outlier field like git_branch, not the
// page size, is the problem) is degraded in place by truncateSessionFields
// instead of being dropped, so the page always contains it and the returned
// cursor always advances past it. Silently emptying the page here would drop
// that session from list_sessions forever, since nothing else ever revisits a
// cursor position once paged past.
func fitSessionsToBudget(r responder, out sessionsDTO) (sessionsDTO, error) {
	originalLen := len(out.Sessions)
	fits := func() bool {
		res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: sessionsSummary(out)}}}
		size, err := r.encodedSize(res, out)
		return err == nil && size <= r.budget
	}
	for len(out.Sessions) > 1 && !fits() {
		out.Sessions = out.Sessions[:len(out.Sessions)-1]
	}
	if len(out.Sessions) == 1 && !fits() {
		if err := truncateSessionFields(&out.Sessions[0], r.budget, fits); err != nil {
			return sessionsDTO{}, err
		}
	}
	if len(out.Sessions) < originalLen && len(out.Sessions) > 0 {
		out.NextCursor = encodeCursor(&store.SessionFeedCursor{ID: out.Sessions[len(out.Sessions)-1].ID})
	}
	return out, nil
}

// truncationMarker flags a field truncateSessionFields shortened to make an
// oversized row fit the response budget. It stays in the value itself so a
// reader never mistakes the shortened text for the field's true content.
const truncationMarker = "...[truncated]"

// truncateSessionFields repeatedly halves the longest of a session's
// degradable string fields (each stripped of any marker from a prior pass
// before measuring, so the marker is never counted toward its own trigger and
// never compounds) until fits reports the row fits, marking the row Truncated.
// Every halving pass makes real progress, so this always terminates: either
// fits eventually reports true, or every degradable field bottoms out at ""
// and the row still does not fit, which the caller reports as an error rather
// than a silently empty page (a floor no plain field truncation can cross:
// see the loud error below).
func truncateSessionFields(s *sessionDTO, budget int, fits func() bool) error {
	fields := []*string{&s.GitBranch, &s.Machine, &s.Username, &s.ProjectKey, &s.ProjectName, &s.ProjectKind}
	for !fits() {
		longest, longestBody := -1, ""
		for i, f := range fields {
			body := strings.TrimSuffix(*f, truncationMarker)
			if len(body) > len(longestBody) {
				longest, longestBody = i, body
			}
		}
		if longest == -1 || longestBody == "" {
			return fmt.Errorf("session %d cannot fit within the %d-byte response budget even after truncating every degradable field", s.ID, budget)
		}
		*fields[longest] = utf8Prefix(longestBody, len(longestBody)/2) + truncationMarker
		s.Truncated = true
	}
	return nil
}

func sessionsSummary(out sessionsDTO) string {
	if out.NextCursor != "" {
		return fmt.Sprintf("list_sessions: %d sessions returned; more available via next_cursor. Full data is in structuredContent.", len(out.Sessions))
	}
	return fmt.Sprintf("list_sessions: %d sessions returned; no more pages. Full data is in structuredContent.", len(out.Sessions))
}
