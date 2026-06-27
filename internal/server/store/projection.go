package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Projection is the parsed, costed result the parse pipeline writes for a
// session. It fully replaces any prior projection rows.
type Projection struct {
	Messages  []ProjMessage
	ToolCalls []ProjToolCall
	Usage     []ProjUsage

	StartedAt        time.Time
	EndedAt          time.Time
	MessageCount     int
	UserMessageCount int
	TotalInput       int64
	TotalOutput      int64
	TotalCacheWrite  int64
	TotalCacheRead   int64
	TotalCostUSD     float64
	CostIncomplete   bool
	ParserVersion    int
}

// ProjMessage is one row for the messages table.
type ProjMessage struct {
	Ordinal       int
	Role          string
	Content       string
	ThinkingText  string
	Model         string
	Timestamp     time.Time
	HasThinking   bool
	HasToolUse    bool
	ContentLength int
}

// ProjToolCall is one row for the tool_calls table. Blob references are left
// nil until the CAS milestone; only sizes and metadata are recorded now.
type ProjToolCall struct {
	MessageOrdinal  int
	CallIndex       int
	ToolName        string
	Category        string
	FilePath        string
	InputBytes      int64
	InputMediaType  string
	ResultBytes     int64
	ResultMediaType string
	ResultStatus    string
	HasResult       bool
}

// ProjUsage is one row for the usage_events table.
type ProjUsage struct {
	MessageOrdinal *int
	Model          string
	Input          int
	Output         int
	CacheWrite     int
	CacheRead      int
	Reasoning      int
	CostUSD        *float64
	OccurredAt     time.Time
	DedupKey       string
}

// LoadRaw returns a session's stored raw bytes.
func (s *Store) LoadRaw(ctx context.Context, sessionID int64) ([]byte, error) {
	var content []byte
	err := s.Pool.QueryRow(ctx, "SELECT content FROM session_raw WHERE session_id = $1", sessionID).Scan(&content)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return content, err
}

// ReparseTarget identifies a session to re-parse.
type ReparseTarget struct {
	ID    int64
	Agent string
}

// SessionsForReparse lists sessions, optionally filtered to one agent.
func (s *Store) SessionsForReparse(ctx context.Context, agent string) ([]ReparseTarget, error) {
	q := "SELECT id, agent FROM sessions"
	var args []any
	if agent != "" {
		q += " WHERE agent = $1"
		args = append(args, agent)
	}
	q += " ORDER BY id"
	rows, err := s.Pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReparseTarget
	for rows.Next() {
		var t ReparseTarget
		if err := rows.Scan(&t.ID, &t.Agent); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ErrStaleProjection reports that the raw bytes changed between the parse and the
// write, so the projection would have overwritten newer data. The caller can
// ignore it: whichever parse matches the current raw bytes wins.
var ErrStaleProjection = errors.New("projection is stale: raw bytes changed since parse")

// WriteProjection replaces a session's derived rows and refreshes its aggregate
// columns, all in one transaction. Announce-managed columns (project, cwd,
// git_branch, visibility) are left untouched.
//
// rawBytes is the byte_len the projection was parsed from. The write locks
// session_raw and aborts with ErrStaleProjection if byte_len no longer matches,
// which serializes concurrent parses (live ingest versus a reparse) so an older,
// shorter parse cannot clobber a newer one.
func (s *Store) WriteProjection(ctx context.Context, sessionID int64, rawBytes int64, p Projection) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var current int64
		if err := tx.QueryRow(ctx,
			"SELECT byte_len FROM session_raw WHERE session_id = $1 FOR UPDATE", sessionID).Scan(&current); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if current != rawBytes {
			return ErrStaleProjection
		}

		for _, q := range []string{
			"DELETE FROM messages WHERE session_id = $1",
			"DELETE FROM tool_calls WHERE session_id = $1",
			"DELETE FROM usage_events WHERE session_id = $1",
		} {
			if _, err := tx.Exec(ctx, q, sessionID); err != nil {
				return err
			}
		}

		if len(p.Messages) > 0 {
			rows := make([][]any, len(p.Messages))
			for i, m := range p.Messages {
				rows[i] = []any{
					sessionID, m.Ordinal, m.Role, m.Content, m.ThinkingText, m.Model,
					nullTime(m.Timestamp), m.HasThinking, m.HasToolUse, m.ContentLength,
				}
			}
			if _, err := tx.CopyFrom(ctx, pgx.Identifier{"messages"},
				[]string{"session_id", "ordinal", "role", "content", "thinking_text", "model",
					"timestamp", "has_thinking", "has_tool_use", "content_length"},
				pgx.CopyFromRows(rows)); err != nil {
				return err
			}
		}

		if len(p.ToolCalls) > 0 {
			rows := make([][]any, len(p.ToolCalls))
			for i, t := range p.ToolCalls {
				var resultBytes, resultMedia, resultStatus any
				if t.HasResult {
					resultBytes, resultMedia, resultStatus = t.ResultBytes, t.ResultMediaType, t.ResultStatus
				}
				rows[i] = []any{
					sessionID, t.MessageOrdinal, t.CallIndex, t.ToolName, t.Category,
					nullString(t.FilePath),
					nil, t.InputBytes, t.InputMediaType, // input_sha256 (nil until CAS), bytes, media
					nil, resultBytes, resultMedia, resultStatus, // result_sha256 (nil until CAS), ...
				}
			}
			if _, err := tx.CopyFrom(ctx, pgx.Identifier{"tool_calls"},
				[]string{"session_id", "message_ordinal", "call_index", "tool_name", "category",
					"file_path", "input_sha256", "input_bytes", "input_media_type",
					"result_sha256", "result_bytes", "result_media_type", "result_status"},
				pgx.CopyFromRows(rows)); err != nil {
				return err
			}
		}

		if len(p.Usage) > 0 {
			rows := make([][]any, len(p.Usage))
			for i, u := range p.Usage {
				var ord any
				if u.MessageOrdinal != nil {
					ord = *u.MessageOrdinal
				}
				var cost any
				if u.CostUSD != nil {
					cost = *u.CostUSD
				}
				rows[i] = []any{
					sessionID, ord, u.Model, u.Input, u.Output, u.CacheWrite, u.CacheRead,
					u.Reasoning, cost, nullTime(u.OccurredAt), u.DedupKey,
				}
			}
			if _, err := tx.CopyFrom(ctx, pgx.Identifier{"usage_events"},
				[]string{"session_id", "message_ordinal", "model", "input_tokens", "output_tokens",
					"cache_write_tokens", "cache_read_tokens", "reasoning_tokens", "cost_usd",
					"occurred_at", "dedup_key"},
				pgx.CopyFromRows(rows)); err != nil {
				return err
			}
		}

		_, err := tx.Exec(ctx,
			`UPDATE sessions SET
			   started_at = $2, ended_at = $3,
			   message_count = $4, user_message_count = $5,
			   total_input_tokens = $6, total_output_tokens = $7,
			   total_cache_write_tokens = $8, total_cache_read_tokens = $9,
			   total_cost_usd = $10, cost_incomplete = $11,
			   parser_version = $12, updated_at = now()
			 WHERE id = $1`,
			sessionID, nullTime(p.StartedAt), nullTime(p.EndedAt),
			p.MessageCount, p.UserMessageCount,
			p.TotalInput, p.TotalOutput, p.TotalCacheWrite, p.TotalCacheRead,
			p.TotalCostUSD, p.CostIncomplete, p.ParserVersion)
		return err
	})
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
