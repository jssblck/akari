package mcpserver

import (
	"context"
	"crypto/sha256"
	"fmt"
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

func fitSessionsToBudget(r responder, out sessionsDTO) sessionsDTO {
	originalLen := len(out.Sessions)
	for len(out.Sessions) > 0 {
		res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: sessionsSummary(out)}}}
		size, err := r.encodedSize(res, out)
		if err == nil && size <= r.budget {
			break
		}
		out.Sessions = out.Sessions[:len(out.Sessions)-1]
	}
	if len(out.Sessions) < originalLen && len(out.Sessions) > 0 {
		out.NextCursor = encodeCursor(&store.SessionFeedCursor{ID: out.Sessions[len(out.Sessions)-1].ID})
	}
	return out
}

func sessionsSummary(out sessionsDTO) string {
	if out.NextCursor != "" {
		return fmt.Sprintf("list_sessions: %d sessions returned; more available via next_cursor. Full data is in structuredContent.", len(out.Sessions))
	}
	return fmt.Sprintf("list_sessions: %d sessions returned; no more pages. Full data is in structuredContent.", len(out.Sessions))
}
