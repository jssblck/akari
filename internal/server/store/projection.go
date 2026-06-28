package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// parseBatchBytes bounds how much raw content one AdvanceProjection call parses
// under the session_raw lock, so catching up a large backlog (after a parser
// upgrade and reparse, or a run of failed parses) does not hold the lock for the
// whole session at once. At least one whole chunk is always processed. It is a
// var so tests can shrink it to force multi-batch catch-up.
var parseBatchBytes int64 = 16 << 20

// ErrParserVersionStale reports that a session was partially parsed by a
// different parser version than the caller's, so incremental parsing cannot
// safely continue from the stored cursor. The fix is a reparse, which resets the
// projection and cursor and replays from zero. The raw bytes are untouched.
var ErrParserVersionStale = errors.New("parser version changed since last parse: reparse required")

// MessageDelta is one message write. Each ordinal is written exactly once: the
// ingest protocol keeps a whole turn inside one chunk, so Content and
// ThinkingText are the complete text of the message, never a fragment to append.
type MessageDelta struct {
	Ordinal      int
	Role         string
	Content      string
	ThinkingText string
	Model        string
	HasThinking  bool
	HasToolUse   bool
	Timestamp    time.Time
}

// ProjToolCall is one tool_calls insert. InputBody holds the bulky input the CAS
// stores; AdvanceProjection writes it and records the sha256 reference. CallUID
// is the agent's call id, used to back-patch the result that arrives on a later
// line (and possibly in a later region, for Claude).
type ProjToolCall struct {
	MessageOrdinal int
	CallIndex      int
	ToolName       string
	Category       string
	FilePath       string
	InputBody      []byte
	InputBytes     int64
	InputMediaType string
	CallUID        string
}

// ToolResultDelta back-patches a tool call's result, matched by call id. Body is
// the bulky result the CAS stores (empty when the result carries no body).
type ToolResultDelta struct {
	CallUID   string
	Body      []byte
	Bytes     int64
	MediaType string
	Status    string
}

// ProjUsage is one usage_events insert. SourceOffset and SourceIndex make the
// insert idempotent (the unique index absorbs a replay via ON CONFLICT).
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
	SourceOffset   int64
	SourceIndex    int
}

// ProjectionDelta is the incremental projection write for one parsed region:
// rows to add and the increments to fold into the session aggregates.
type ProjectionDelta struct {
	Messages    []MessageDelta
	ToolCalls   []ProjToolCall
	ToolResults []ToolResultDelta
	Usage       []ProjUsage

	MessagesAdded     int
	UserMessagesAdded int
	AddInput          int64
	AddOutput         int64
	AddCacheWrite     int64
	AddCacheRead      int64
	AddCostUSD        float64
	CostIncomplete    bool
	Started           time.Time
	Ended             time.Time
}

// ReduceFunc parses a raw region beginning at baseOffset, given the prior
// serialized parser state, and returns the new state plus the projection delta.
// It is pure CPU: AdvanceProjection runs it inside the parse transaction, so it
// must not perform I/O.
type ReduceFunc func(state, region []byte, baseOffset int64) (newState []byte, d ProjectionDelta, err error)

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

// AdvanceProjection parses the next unparsed region of a session and applies it
// incrementally. It locks the session_raw row, reads up to parseBatchBytes of raw
// content past the parse cursor, runs reduce, applies the delta, and advances the
// cursor and parser state, all in one transaction. It returns the new parse
// cursor and whether the session is now fully parsed.
//
// It is a no-op (caughtUp=true) when the cursor already equals the stored length.
// It returns ErrParserVersionStale when a partially parsed session was last
// touched by a different parser version, leaving the projection for a reparse to
// rebuild. The raw bytes are never modified here.
func (s *Store) AdvanceProjection(ctx context.Context, sessionID int64, parserVersion int, reduce ReduceFunc) (parsedTo int64, caughtUp bool, err error) {
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Lock the parent session row before session_raw. DeleteSession locks
		// sessions first and cascades into session_raw, so taking the two rows in
		// that same order here keeps a concurrent delete and parse from deadlocking.
		if err := lockSession(ctx, tx, sessionID); err != nil {
			return err
		}
		var byteLen, parsedLen int64
		var stateJSON []byte
		var stateVer int
		if err := tx.QueryRow(ctx,
			`SELECT byte_len, parsed_byte_len, parse_state, parse_state_version
			   FROM session_raw WHERE session_id = $1 FOR UPDATE`, sessionID).
			Scan(&byteLen, &parsedLen, &stateJSON, &stateVer); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("load parse cursor for session %d: %w", sessionID, err)
		}
		if parsedLen >= byteLen {
			parsedTo, caughtUp = parsedLen, true
			return nil
		}
		// A session parsed from byte 0 adopts the caller's version; one parsed past
		// 0 by a different version cannot be resumed and needs a reparse.
		if parsedLen > 0 && stateVer != parserVersion {
			return ErrParserVersionStale
		}

		region, regionEnd, err := readRawRegion(ctx, tx, sessionID, parsedLen, parseBatchBytes)
		if err != nil {
			return err
		}

		newState, d, err := reduce(stateJSON, region, parsedLen)
		if err != nil {
			return fmt.Errorf("parse session %d region [%d,%d): %w", sessionID, parsedLen, regionEnd, err)
		}
		if err := applyDelta(ctx, tx, sessionID, d); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx,
			`UPDATE session_raw
			    SET parsed_byte_len = $2, parse_state = $3, parse_state_version = $4, parse_error = ''
			  WHERE session_id = $1`,
			sessionID, regionEnd, newState, parserVersion); err != nil {
			return fmt.Errorf("advance parse cursor for session %d to %d: %w", sessionID, regionEnd, err)
		}
		if err := applyAggregates(ctx, tx, sessionID, parserVersion, d); err != nil {
			return err
		}

		parsedTo, caughtUp = regionEnd, regionEnd >= byteLen
		return nil
	})
	return parsedTo, caughtUp, err
}

// ApplyProjectionDelta applies a projection delta to a session in one
// transaction (message upserts, tool-call inserts with their CAS bodies,
// tool-result back-patches, and usage inserts) without advancing the parse
// cursor or the session aggregates. AdvanceProjection wraps applyDelta with that
// bookkeeping; this exposes just the row writes, which is the seam tests use to
// exercise the projection and CAS directly.
func (s *Store) ApplyProjectionDelta(ctx context.Context, sessionID int64, d ProjectionDelta) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		return applyDelta(ctx, tx, sessionID, d)
	})
}

// readRawRegion concatenates the raw chunks for one parse batch: the chunks whose
// start falls in [from, from+cap). The bound is in SQL, so a backlog catch-up of
// many AdvanceProjection calls fetches only each batch's chunks rather than
// rescanning the whole tail every time. Chunks are contiguous and line aligned,
// so the returned region always ends on a JSONL line boundary, and the chunk at
// `from` always qualifies (so a batch is never empty when bytes remain). It
// returns the bytes and the offset just past them.
func readRawRegion(ctx context.Context, tx pgx.Tx, sessionID, from int64, cap int64) ([]byte, int64, error) {
	rows, err := tx.Query(ctx,
		`SELECT byte_offset, byte_len, content
		   FROM session_raw_chunks
		  WHERE session_id = $1 AND byte_offset >= $2 AND byte_offset < $2 + $3
		  ORDER BY byte_offset`, sessionID, from, cap)
	if err != nil {
		return nil, 0, fmt.Errorf("read raw chunks for session %d from offset %d: %w", sessionID, from, err)
	}
	defer rows.Close()

	var region []byte
	end := from
	for rows.Next() {
		var off, length int64
		var content []byte
		if err := rows.Scan(&off, &length, &content); err != nil {
			return nil, 0, fmt.Errorf("scan raw chunk for session %d: %w", sessionID, err)
		}
		if off != end {
			return nil, 0, fmt.Errorf("raw chunk gap for session %d: expected offset %d, got %d", sessionID, end, off)
		}
		region = append(region, content...)
		end = off + length
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate raw chunks for session %d: %w", sessionID, err)
	}
	return region, end, nil
}

// applyDelta writes one region's rows: message upserts, tool-call inserts (with
// their input bodies in the CAS), tool-result back-patches, and usage inserts.
func applyDelta(ctx context.Context, tx pgx.Tx, sessionID int64, d ProjectionDelta) error {
	// Each ordinal is inserted once: a turn is folded whole within the region that
	// carries it, so there is no in-place content rewrite and no quadratic append.
	// The ON CONFLICT DO NOTHING is a replay guard only (a region is parsed once,
	// since the cursor advances in the same transaction, and a reparse deletes
	// these rows first), so a retried region never duplicates or rewrites a row.
	for _, m := range d.Messages {
		if _, err := tx.Exec(ctx,
			`INSERT INTO messages
			   (session_id, ordinal, role, content, thinking_text, model, timestamp, has_thinking, has_tool_use)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			 ON CONFLICT (session_id, ordinal) DO NOTHING`,
			sessionID, m.Ordinal, m.Role, m.Content, m.ThinkingText, m.Model,
			nullTime(m.Timestamp), m.HasThinking, m.HasToolUse); err != nil {
			return fmt.Errorf("write message %d for session %d: %w", m.Ordinal, sessionID, err)
		}
	}

	for _, t := range d.ToolCalls {
		var inputSHA, inputMedia any
		if len(t.InputBody) > 0 {
			sha, err := writeBlobTx(ctx, tx, t.InputBody, t.InputMediaType)
			if err != nil {
				return fmt.Errorf("write tool input blob for session %d call %d/%d: %w", sessionID, t.MessageOrdinal, t.CallIndex, err)
			}
			inputSHA, inputMedia = sha, t.InputMediaType
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO tool_calls
			   (session_id, message_ordinal, call_index, tool_name, category, file_path,
			    input_sha256, input_bytes, input_media_type, call_uid)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			 ON CONFLICT (session_id, message_ordinal, call_index) DO NOTHING`,
			sessionID, t.MessageOrdinal, t.CallIndex, t.ToolName, t.Category, nullString(t.FilePath),
			inputSHA, t.InputBytes, inputMedia, nullString(t.CallUID)); err != nil {
			return fmt.Errorf("insert tool call %d/%d for session %d: %w", t.MessageOrdinal, t.CallIndex, sessionID, err)
		}
	}

	for _, tr := range d.ToolResults {
		if tr.CallUID == "" {
			continue
		}
		var resultSHA any
		if len(tr.Body) > 0 {
			sha, err := writeBlobTx(ctx, tx, tr.Body, tr.MediaType)
			if err != nil {
				return fmt.Errorf("write tool result blob for session %d call %q: %w", sessionID, tr.CallUID, err)
			}
			resultSHA = sha
		}
		media := tr.MediaType
		if media == "" {
			media = "text/plain"
		}
		if _, err := tx.Exec(ctx,
			`UPDATE tool_calls
			    SET result_sha256 = $3, result_bytes = $4, result_media_type = $5, result_status = $6
			  WHERE session_id = $1 AND call_uid = $2`,
			sessionID, tr.CallUID, resultSHA, tr.Bytes, media, tr.Status); err != nil {
			return fmt.Errorf("back-patch tool result for session %d call %q: %w", sessionID, tr.CallUID, err)
		}
	}

	for _, u := range d.Usage {
		var ord, cost any
		if u.MessageOrdinal != nil {
			ord = *u.MessageOrdinal
		}
		if u.CostUSD != nil {
			cost = *u.CostUSD
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO usage_events
			   (session_id, message_ordinal, model, input_tokens, output_tokens,
			    cache_write_tokens, cache_read_tokens, reasoning_tokens, cost_usd,
			    occurred_at, dedup_key, source_offset, source_index)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			 ON CONFLICT DO NOTHING`,
			sessionID, ord, u.Model, u.Input, u.Output, u.CacheWrite, u.CacheRead,
			u.Reasoning, cost, nullTime(u.OccurredAt), u.DedupKey, u.SourceOffset, u.SourceIndex); err != nil {
			return fmt.Errorf("insert usage event for session %d at offset %d: %w", sessionID, u.SourceOffset, err)
		}
	}
	return nil
}

// applyAggregates folds a region's increments into the session rollups. Token and
// cost totals add; the span widens by LEAST/GREATEST (both ignore NULLs, so a
// region with no timestamps leaves the bounds unchanged); cost_incomplete is
// sticky once any unpriced model is seen.
func applyAggregates(ctx context.Context, tx pgx.Tx, sessionID int64, parserVersion int, d ProjectionDelta) error {
	_, err := tx.Exec(ctx,
		`UPDATE sessions SET
		   message_count = message_count + $2,
		   user_message_count = user_message_count + $3,
		   total_input_tokens = total_input_tokens + $4,
		   total_output_tokens = total_output_tokens + $5,
		   total_cache_write_tokens = total_cache_write_tokens + $6,
		   total_cache_read_tokens = total_cache_read_tokens + $7,
		   total_cost_usd = total_cost_usd + $8,
		   cost_incomplete = cost_incomplete OR $9,
		   started_at = LEAST(started_at, $10),
		   ended_at = GREATEST(ended_at, $11),
		   parser_version = $12,
		   updated_at = now()
		 WHERE id = $1`,
		sessionID, d.MessagesAdded, d.UserMessagesAdded,
		d.AddInput, d.AddOutput, d.AddCacheWrite, d.AddCacheRead,
		d.AddCostUSD, d.CostIncomplete,
		nullTime(d.Started), nullTime(d.Ended), parserVersion)
	if err != nil {
		return fmt.Errorf("update aggregates for session %d: %w", sessionID, err)
	}
	return nil
}

// ResetProjectionForReparse clears a session's parser-owned projection rows and
// its aggregates, and rewinds the parse cursor to zero at the given version,
// keeping the raw bytes and their hash. The reparse loop then replays the stored
// raw through the parser from scratch. Attachments are not parser-owned and are
// left intact.
func (s *Store) ResetProjectionForReparse(ctx context.Context, sessionID int64, parserVersion int) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Parent session first, then session_raw: the same order DeleteSession takes,
		// so a concurrent delete cannot deadlock with a reparse.
		if err := lockSession(ctx, tx, sessionID); err != nil {
			return err
		}
		var dummy int64
		if err := tx.QueryRow(ctx,
			`SELECT session_id FROM session_raw WHERE session_id = $1 FOR UPDATE`, sessionID).Scan(&dummy); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return fmt.Errorf("lock session_raw for reparse of session %d: %w", sessionID, err)
		}
		for _, q := range []string{
			"DELETE FROM messages WHERE session_id = $1",
			"DELETE FROM tool_calls WHERE session_id = $1",
			"DELETE FROM usage_events WHERE session_id = $1",
		} {
			if _, err := tx.Exec(ctx, q, sessionID); err != nil {
				return fmt.Errorf("clear projection for reparse of session %d (%s): %w", sessionID, q, err)
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE session_raw
			    SET parsed_byte_len = 0, parse_state = '{}'::jsonb,
			        parse_state_version = $2, parse_error = ''
			  WHERE session_id = $1`, sessionID, parserVersion); err != nil {
			return fmt.Errorf("rewind parse cursor for session %d: %w", sessionID, err)
		}
		return resetSessionAggregates(ctx, tx, sessionID)
	})
}

// resetSessionAggregates zeroes a session's rollups so a from-scratch parse can
// re-accumulate them.
func resetSessionAggregates(ctx context.Context, tx pgx.Tx, sessionID int64) error {
	_, err := tx.Exec(ctx,
		`UPDATE sessions SET
		   message_count = 0, user_message_count = 0,
		   total_input_tokens = 0, total_output_tokens = 0,
		   total_cache_write_tokens = 0, total_cache_read_tokens = 0,
		   total_cost_usd = 0, cost_incomplete = FALSE,
		   started_at = NULL, ended_at = NULL,
		   updated_at = now()
		 WHERE id = $1`, sessionID)
	if err != nil {
		return fmt.Errorf("reset aggregates for session %d: %w", sessionID, err)
	}
	return nil
}

// lockSession takes the row lock on the parent session before the parse and reset
// paths lock session_raw and update the session aggregates. DeleteSession locks
// the session row (its DELETE) and then cascades into session_raw and the child
// projection tables, so acquiring the session row first here gives one global lock
// order (session, then session_raw) and rules out the parent/child deadlock where
// one transaction holds session_raw while waiting on sessions and another holds
// sessions while waiting on session_raw. AppendChunk stays out of this: it locks
// session_raw but never the session row, so it cannot close such a cycle.
func lockSession(ctx context.Context, tx pgx.Tx, sessionID int64) error {
	var id int64
	if err := tx.QueryRow(ctx, `SELECT id FROM sessions WHERE id = $1 FOR UPDATE`, sessionID).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("lock session %d: %w", sessionID, err)
	}
	return nil
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
