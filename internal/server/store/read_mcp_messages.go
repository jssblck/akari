package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// MCPMessage is a transcript row whose large text fields may be previews. The
// byte lengths always describe the stored values, which lets the MCP layer
// publish a retrievable reference without first materializing the full field.
type MCPMessage struct {
	Message
	ContentBytes          int64
	ThinkingTextBytes     int64
	ContentSHA256         string
	ThinkingTextSHA256    string
	ContentTruncated      bool
	ThinkingTextTruncated bool
}

// MCPMessagesAfter returns an ordinal-keyset page whose worst-case JSON string
// expansion stays within byteBudget. PostgreSQL applies the cumulative bound
// before sending text over the connection, so many medium messages and one huge
// message both keep server memory proportional to the configured response cap.
// A first row that cannot fit is returned as previews so the caller can advance
// the cursor and attach authenticated references to the stored fields.
func (s *Store) MCPMessagesAfter(ctx context.Context, sessionID int64, after *int, limit int, byteBudget int64, previewChars int) ([]MCPMessage, bool, bool, error) {
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	if byteBudget <= 0 {
		byteBudget = 1
	}
	if previewChars <= 0 {
		previewChars = 1
	}
	afterSet, afterOrdinal := after != nil, 0
	if after != nil {
		afterOrdinal = *after
	}

	rows, err := s.Pool.Query(ctx, `
		WITH candidates AS MATERIALIZED (
			SELECT m.ordinal, m.content_length::bigint AS content_bytes,
			       octet_length(m.thinking_text)::bigint AS thinking_text_bytes,
			       1024::bigint + 6 * (
			           m.content_length::bigint + octet_length(m.thinking_text)::bigint +
			           octet_length(m.role)::bigint + octet_length(m.model)::bigint
			       ) AS worst_bytes
			  FROM messages m
			 WHERE m.session_id = $1 AND (NOT $2::boolean OR m.ordinal > $3)
			 ORDER BY m.ordinal
			 LIMIT $4 + 1
		), sized AS (
			SELECT c.*,
			       row_number() OVER (ORDER BY c.ordinal) AS row_num,
			       sum(c.worst_bytes) OVER (ORDER BY c.ordinal) AS running_bytes,
			       count(*) OVER () AS candidate_count
			  FROM candidates c
		)
		SELECT m.ordinal, m.role,
		       CASE WHEN s.worst_bytes > $5 THEN left(m.content, $6) ELSE m.content END,
		       CASE WHEN s.worst_bytes > $5 THEN left(m.thinking_text, $6) ELSE m.thinking_text END,
		       m.model, m.has_thinking, m.has_tool_use, m.timestamp,
		       s.content_bytes, s.thinking_text_bytes,
		       CASE WHEN s.worst_bytes > $5 AND s.content_bytes > 0 THEN m.content_sha256 ELSE '' END,
		       CASE WHEN s.worst_bytes > $5 AND s.thinking_text_bytes > 0 THEN m.thinking_text_sha256 ELSE '' END,
		       s.worst_bytes > $5 AND s.content_bytes > 0,
		       s.worst_bytes > $5 AND s.thinking_text_bytes > 0,
		       s.candidate_count
		  FROM sized s
		  JOIN messages m ON m.session_id = $1 AND m.ordinal = s.ordinal
		 WHERE s.row_num <= $4 AND (s.running_bytes <= $5 OR s.row_num = 1)
		 ORDER BY m.ordinal`,
		sessionID, afterSet, afterOrdinal, limit, byteBudget, previewChars)
	if err != nil {
		return nil, false, false, fmt.Errorf("query bounded messages for session %d: %w", sessionID, err)
	}
	defer rows.Close()

	var out []MCPMessage
	var candidateCount int
	fieldTruncated := false
	for rows.Next() {
		var m MCPMessage
		if err := rows.Scan(
			&m.Ordinal, &m.Role, &m.Content, &m.ThinkingText, &m.Model,
			&m.HasThinking, &m.HasToolUse, &m.Timestamp,
			&m.ContentBytes, &m.ThinkingTextBytes,
			&m.ContentSHA256, &m.ThinkingTextSHA256,
			&m.ContentTruncated, &m.ThinkingTextTruncated, &candidateCount,
		); err != nil {
			return nil, false, false, fmt.Errorf("scan bounded message for session %d: %w", sessionID, err)
		}
		fieldTruncated = fieldTruncated || m.ContentTruncated || m.ThinkingTextTruncated
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, false, fmt.Errorf("iterate bounded messages for session %d: %w", sessionID, err)
	}
	hasMore := candidateCount > len(out)
	byteTruncated := fieldTruncated || (hasMore && len(out) < limit)
	return out, hasMore, byteTruncated, nil
}

// MessageText reads one message field for an authenticated MCP resource. The
// fixed field switch keeps the query static and rejects invented URI suffixes.
func (s *Store) MessageText(ctx context.Context, sessionID int64, ordinal int, field, sha256 string) (string, error) {
	query := ""
	switch field {
	case "content":
		query = `SELECT content FROM messages
			WHERE session_id = $1 AND ordinal = $2
			  AND content_sha256 = $3`
	case "thinking":
		query = `SELECT thinking_text FROM messages
			WHERE session_id = $1 AND ordinal = $2
			  AND thinking_text_sha256 = $3`
	default:
		return "", ErrNotFound
	}
	var text string
	err := s.Pool.QueryRow(ctx, query, sessionID, ordinal, sha256).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read message %d field %s for session %d: %w", ordinal, field, sessionID, err)
	}
	return text, nil
}
