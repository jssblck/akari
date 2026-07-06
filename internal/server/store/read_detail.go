package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
)

// scanDetail loads one session with its project into a SessionDetail by an arbitrary
// WHERE clause. Every displayed field comes from this one sessions-row read, so the
// token split and the cache saving are always one consistent snapshot. The rollups
// (including total_cache_savings_usd) are re-folded whole by every rebuild, so there is
// no read-side backfill or pricing-marker dance: a reprice ships as a parse.Epoch bump
// and the epoch rebuild re-prices the corpus.
func (s *Store) scanDetail(ctx context.Context, q querier, where string, arg any) (SessionDetail, error) {
	var d SessionDetail
	err := q.QueryRow(ctx,
		`SELECT s.id, s.agent, s.machine, s.git_branch, u.username,
		        s.message_count, s.user_message_count, s.model_fallback_count,
		        s.total_input_tokens, s.total_output_tokens,
		        s.total_cache_write_tokens, s.total_cache_read_tokens,
		        s.total_cost_usd, s.cost_incomplete, s.visibility, s.public_id,
		        s.started_at, s.ended_at, s.last_active_at,
		        s.user_id, s.project_id, p.remote_key, p.display_name, p.kind, s.cwd, s.parent_session_id,
		        s.total_cache_savings_usd, s.cache_savings_incomplete,
		        coalesce(title.content, '')
		   FROM sessions s
		   JOIN users u ON u.id = s.user_id
		   JOIN projects p ON p.id = s.project_id
		   `+titleLateralSQL+`
		  WHERE `+where,
		arg).Scan(
		&d.ID, &d.Agent, &d.Machine, &d.GitBranch, &d.Username,
		&d.MessageCount, &d.UserMessageCount, &d.ModelFallbackCount,
		&d.TotalInput, &d.TotalOutput, &d.TotalCacheWrite, &d.TotalCacheRead,
		&d.TotalCostUSD, &d.CostIncomplete, &d.Visibility, &d.PublicID,
		&d.StartedAt, &d.EndedAt, &d.LastActiveAt,
		&d.OwnerID, &d.ProjectID, &d.ProjectKey, &d.ProjectName, &d.ProjectKind, &d.Cwd, &d.ParentID,
		&d.TotalCacheSavingsUSD, &d.CacheSavingsIncomplete,
		&d.Title)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionDetail{}, ErrNotFound
	}
	if err != nil {
		return SessionDetail{}, err
	}
	d.Title = squashSpaces(d.Title)
	return d, nil
}

// SessionDetailByID loads a session by numeric id.
func (s *Store) SessionDetailByID(ctx context.Context, id int64) (SessionDetail, error) {
	return s.scanDetail(ctx, s.Pool, "s.id = $1", id)
}

// SessionDetailByPublicID loads a published session by its public id.
func (s *Store) SessionDetailByPublicID(ctx context.Context, publicID string) (SessionDetail, error) {
	return s.scanDetail(ctx, s.Pool, "s.public_id = $1 AND s.visibility = 'public'", publicID)
}

// MessageCount returns a session's current message count from its rollup.
func (s *Store) MessageCount(ctx context.Context, sessionID int64) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, "SELECT message_count FROM sessions WHERE id = $1", sessionID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("read message count for session %d: %w", sessionID, err)
	}
	return n, nil
}

// Messages returns a session's whole transcript in order, each row carrying its per-turn usage
// (Usage) and duplicate-prompt verdict (DuplicatePrompt) folded in the same read. The web renderer
// wants the full session in one pass; bounded readers (the MCP transcript window) use MessagesAfter
// instead so peak memory does not scale with session size.
//
// Both the duplicate-prompt verdict and the per-turn usage are read from stored per-message rows
// (duplicate_prompt on the messages row and the message_turn_usage rollup, both materialized by
// the rebuild's in-memory fold), not folded from whole-session windows here. So the live body
// fragment (handleSessionBody) re-fetching this on every SSE append reads bounded indexed rows
// and does no growing whole-session usage aggregation or message-window scan for either.
func (s *Store) Messages(ctx context.Context, sessionID int64) ([]Message, error) {
	return s.scanMessages(ctx, s.Pool, sessionID, messagesFullQuery, sessionID)
}

// MessagesAfter returns the next window of a session's transcript ordered by
// ordinal, starting strictly after the given ordinal (after == nil for the first
// window). It pages by keyset on ordinal rather than OFFSET: each call walks the
// messages primary key (session_id, ordinal) straight to the resume point and reads
// only the next `limit` rows, so reading a whole session window by window costs
// O(N), not the O(N^2/limit) an OFFSET walk would (Postgres re-skips the already
// returned prefix on every page). limit is clamped to [1, 2000].
//
// It deliberately does NOT fold the per-turn usage or the duplicate-prompt flag (both left empty on
// the returned messages): those require a whole-session scan, and running them here per page would
// make a client paging a long transcript pay O(N) whole-session work on each of O(N/limit) pages.
// The MCP transcript window (its only caller) renders neither, so the bounded read stays bounded.
func (s *Store) MessagesAfter(ctx context.Context, sessionID int64, after *int, limit int) ([]Message, error) {
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	if after == nil {
		return s.scanMessages(ctx, s.Pool, sessionID,
			messagesWindowQuery+` ORDER BY m.ordinal LIMIT $2`,
			sessionID, limit)
	}
	return s.scanMessages(ctx, s.Pool, sessionID,
		messagesWindowQuery+` AND m.ordinal > $2 ORDER BY m.ordinal LIMIT $3`,
		sessionID, *after, limit)
}

// scanMessages runs a transcript read and scans its rows into Messages. The querier is the pool
// for a standalone read or a transaction when several windowed reads must see one snapshot
// (read_transcript_page.go). sessionID is carried only to give the error path context: a cursor,
// network, or cancellation failure mid-read then names the session and the operation rather than
// surfacing a bare driver error to the handler.
func (s *Store) scanMessages(ctx context.Context, q querier, sessionID int64, query string, args ...any) ([]Message, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages for session %d: %w", sessionID, err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var hasUsage bool
		var u TurnUsage
		var costSum *float64
		var costCount int64
		if err := rows.Scan(&m.Ordinal, &m.Role, &m.Content, &m.ThinkingText, &m.Model,
			&m.HasThinking, &m.HasToolUse, &m.ThinkingBytes, &m.Timestamp,
			&m.PromptShort, &m.PromptNoCode, &m.PromptDigest, &m.PromptFactsCurrent,
			&m.DuplicatePrompt,
			&hasUsage, &u.Input, &u.Output, &u.CacheRead, &u.CacheWrite, &u.Reasoning, &u.ContextTokens,
			&costSum, &costCount, &u.CostIncomplete); err != nil {
			return nil, fmt.Errorf("scan message for session %d: %w", sessionID, err)
		}
		if hasUsage {
			// count(cost_usd) == 0 means every contributing row was unpriced, so the summed cost is
			// meaningless: leave CostUSD nil rather than show a summed zero that reads as free. A mixed
			// group keeps its priced partial (sum ignores NULLs), the all-unpriced-is-nil rule; the
			// cost_incomplete flag then marks that partial as a lower bound.
			if costCount > 0 {
				u.CostUSD = costSum
			}
			usage := u
			m.Usage = &usage
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages for session %d: %w", sessionID, err)
	}
	return out, nil
}

// toolCallsQuery reads all of a session's tool calls, shared by the pool-backed read
// and the session snapshot's in-transaction read.
const toolCallsQuery = `SELECT message_ordinal, call_index, tool_name, coalesce(category,''), coalesce(file_path,''), coalesce(file_rel_path,''), coalesce(detail,''),
	        coalesce(input_sha256,''), coalesce(input_bytes,0), coalesce(input_media_type,''),
	        coalesce(result_sha256,''), coalesce(result_bytes,0), coalesce(result_media_type,''), coalesce(result_status,'')
	   FROM tool_calls WHERE session_id = $1 ORDER BY message_ordinal, call_index`

// ToolCalls returns all of a session's tool calls as metadata, for the web
// renderer. Bounded readers pass a message-ordinal range to ToolCallsInRange.
func (s *Store) ToolCalls(ctx context.Context, sessionID int64) ([]ToolCallView, error) {
	return s.scanToolCalls(ctx, s.Pool, toolCallsQuery, sessionID)
}

// toolCallsInRangeQuery reads the tool calls hanging on messages in an inclusive
// ordinal window, shared by the pool-backed range read and the transcript page's
// in-transaction read (which must see the same snapshot as the window's messages).
const toolCallsInRangeQuery = `SELECT message_ordinal, call_index, tool_name, coalesce(category,''), coalesce(file_path,''), coalesce(file_rel_path,''), coalesce(detail,''),
	        coalesce(input_sha256,''), coalesce(input_bytes,0), coalesce(input_media_type,''),
	        coalesce(result_sha256,''), coalesce(result_bytes,0), coalesce(result_media_type,''), coalesce(result_status,'')
	   FROM tool_calls WHERE session_id = $1 AND message_ordinal BETWEEN $2 AND $3
	   ORDER BY message_ordinal, call_index`

// ToolCallsInRange returns the tool calls hanging on messages in the inclusive
// ordinal window [minOrdinal, maxOrdinal], so a bounded transcript read fetches
// only the calls for the messages it returned rather than the whole session.
func (s *Store) ToolCallsInRange(ctx context.Context, sessionID int64, minOrdinal, maxOrdinal int) ([]ToolCallView, error) {
	return s.scanToolCalls(ctx, s.Pool, toolCallsInRangeQuery, sessionID, minOrdinal, maxOrdinal)
}

func (s *Store) scanToolCalls(ctx context.Context, q querier, query string, args ...any) ([]ToolCallView, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ToolCallView
	for rows.Next() {
		var t ToolCallView
		if err := rows.Scan(&t.MessageOrdinal, &t.CallIndex, &t.ToolName, &t.Category, &t.FilePath, &t.FileRelPath, &t.Detail,
			&t.InputSHA, &t.InputBytes, &t.InputMediaType,
			&t.ResultSHA, &t.ResultBytes, &t.ResultMediaType, &t.ResultStatus); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TurnUsage is one message-turn's rolled-up usage, folded across the (possibly several,
// streamed) usage_events rows that share a message_ordinal. It rides on Message.Usage, folded
// in the transcript read's usage_agg CTE (see messageReadCTEs) rather than fetched as a separate
// per-ordinal map. Input, Output, CacheRead, CacheWrite, and Reasoning are the summed token
// classes.
//
// CostUSD is the summed cost, but nil when EVERY contributing row's cost_usd was NULL: an
// unpriced model has no cost to show, and a summed zero would read as "this turn was free"
// rather than "we could not price it". A turn that mixes priced and unpriced rows returns the
// priced partial (a lower bound), matching how the session-level cost total treats an
// incomplete price.
//
// CostIncomplete is true when the turn folded in a usage row that carried real token volume but no
// price, so CostUSD (when present) is a lower bound: the summed cost covers only the priced subset
// of the token classes the card shows beside it. It is the per-turn shape of the session and
// analytics costIncompleteExpr, so the turn's cost stamp gets the same "$X+" lower-bound marker
// those figures do rather than an exact-looking cost next to unpriced tokens. It is false for a
// fully-priced turn and for a fully-unpriced one (where CostUSD is nil and the card reads
// "unpriced" instead).
//
// ContextTokens is the turn's context occupancy: Input + CacheRead + CacheWrite, output
// EXCLUDED. It is the size of the prompt presented that turn (what the model had to read),
// not the cumulative spend: output tokens are what the model produced, so they are not part of
// the context it carried in. This mirrors gatherContextHealth's definition exactly (the same
// three-class sum), so the per-message context stamp and the session's peak-context signal are
// measuring the same thing at two granularities. The divergence between this ordinal-grouped,
// attributed-only fold and the signal's raw fold is documented on the usage_agg CTE in
// messageReadCTEs and pinned by TestMessagesTurnUsageDivergesFromContextFold.
type TurnUsage struct {
	Input, Output, CacheRead, CacheWrite, Reasoning int64
	CostUSD                                         *float64
	CostIncomplete                                  bool
	ContextTokens                                   int64
}

// DuplicateCallUIDCount returns how many of a session's tool-call ids appear on more
// than one row. The GROUP BY runs in the database against the (session_id, call_uid)
// index, so the result is a bounded scalar and the session view can flag a repeated
// id without loading or grouping the calls in process memory. It is normally zero; a
// non-zero count means the transcript replayed a turn (a resumed or compacted Claude
// session repeats a tool_use id), which the view surfaces as a chip so a genuinely
// malformed id reuse is visible rather than silent.
func (s *Store) DuplicateCallUIDCount(ctx context.Context, sessionID int64) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx,
		`SELECT count(*) FROM (
		   SELECT 1 FROM tool_calls
		    WHERE session_id = $1 AND call_uid IS NOT NULL
		    GROUP BY call_uid HAVING count(*) > 1
		 ) dups`, sessionID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count duplicate call ids for session %d: %w", sessionID, err)
	}
	return n, nil
}

// AttachmentView is one attachment (today a lifted image) rendered under its
// message: the blob key plus enough metadata to show or link the image without
// fetching it. The bytes are served on demand through the session-scoped blob route.
type AttachmentView struct {
	MessageOrdinal int
	SHA256         string
	MediaType      string
	ByteLen        int64
	Filename       string
}

// Attachments returns all of a session's attachments, ordered by the message they
// hang on, for the web renderer. Bounded readers pass an ordinal range to
// AttachmentsInRange.
func (s *Store) Attachments(ctx context.Context, sessionID int64) ([]AttachmentView, error) {
	return s.scanAttachments(ctx, s.Pool,
		`SELECT coalesce(message_ordinal, 0), sha256, coalesce(media_type,''), coalesce(byte_len,0), coalesce(filename,'')
		   FROM attachments WHERE session_id = $1 ORDER BY message_ordinal, id`, sessionID)
}

// attachmentsInRangeQuery reads the attachments hanging on messages in an inclusive
// ordinal window, shared like toolCallsInRangeQuery.
const attachmentsInRangeQuery = `SELECT coalesce(message_ordinal, 0), sha256, coalesce(media_type,''), coalesce(byte_len,0), coalesce(filename,'')
	   FROM attachments WHERE session_id = $1 AND message_ordinal BETWEEN $2 AND $3
	   ORDER BY message_ordinal, id`

// AttachmentsInRange returns the attachments hanging on messages in the inclusive
// ordinal window [minOrdinal, maxOrdinal], so a bounded transcript read fetches
// only the attachments for the messages it returned.
func (s *Store) AttachmentsInRange(ctx context.Context, sessionID int64, minOrdinal, maxOrdinal int) ([]AttachmentView, error) {
	return s.scanAttachments(ctx, s.Pool, attachmentsInRangeQuery, sessionID, minOrdinal, maxOrdinal)
}

func (s *Store) scanAttachments(ctx context.Context, q querier, query string, args ...any) ([]AttachmentView, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query attachments: %w", err)
	}
	defer rows.Close()
	var out []AttachmentView
	for rows.Next() {
		var a AttachmentView
		if err := rows.Scan(&a.MessageOrdinal, &a.SHA256, &a.MediaType, &a.ByteLen, &a.Filename); err != nil {
			return nil, fmt.Errorf("scan attachment row: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate attachments: %w", err)
	}
	return out, nil
}

// ModelFallback is one recorded model fallback: a Claude Fable turn the safety classifier
// declined and re-served on a lower model (see migration 0034). The row is merged from the
// several transcript lines of one logical fallback, so a field may be unset when only one
// source line was seen: MessageOrdinal is nil on a system-only row, the declined token
// counts are nil until the assistant side merged in, and RefusalCategory/RefusalExplanation
// are empty until the system entry merged in. FromModel and ToModel are the models the turn
// fell FROM (Fable) and TO (the served lower model).
type ModelFallback struct {
	MessageOrdinal     *int
	FromModel          string
	ToModel            string
	Trigger            string
	RefusalCategory    string
	RefusalExplanation string
	DeclinedInput      *int64
	DeclinedOutput     *int64
	DeclinedCacheWrite *int64
	DeclinedCacheRead  *int64
	OccurredAt         *time.Time
	DedupKey           string
}

// SessionModelFallbacks returns a session's recorded model fallbacks in a stable order
// (by when they occurred, then by dedup_key so rows with no timestamp still order
// deterministically), capped at limit rows so the read stays bounded on a pathological
// session. It reads the merged model_fallbacks rows the projection built. A limit of zero
// or less means no cap; callers on hot paths pass ModelFallbackListCap.
func (s *Store) SessionModelFallbacks(ctx context.Context, sessionID int64, limit int) ([]ModelFallback, error) {
	query := `SELECT message_ordinal, from_model, to_model, trigger,
	        COALESCE(refusal_category, ''), COALESCE(refusal_explanation, ''),
	        declined_input_tokens, declined_output_tokens, declined_cache_write_tokens,
	        declined_cache_read_tokens, occurred_at, dedup_key
	   FROM model_fallbacks WHERE session_id = $1
	  ORDER BY occurred_at, dedup_key`
	args := []any{sessionID}
	if limit > 0 {
		query += ` LIMIT $2`
		args = append(args, limit)
	}
	rows, err := s.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query model fallbacks for session %d: %w", sessionID, err)
	}
	defer rows.Close()
	var out []ModelFallback
	for rows.Next() {
		var f ModelFallback
		if err := rows.Scan(&f.MessageOrdinal, &f.FromModel, &f.ToModel, &f.Trigger,
			&f.RefusalCategory, &f.RefusalExplanation,
			&f.DeclinedInput, &f.DeclinedOutput, &f.DeclinedCacheWrite, &f.DeclinedCacheRead,
			&f.OccurredAt, &f.DedupKey); err != nil {
			return nil, fmt.Errorf("scan model fallback row: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model fallbacks: %w", err)
	}
	return out, nil
}

// SessionRawTo streams a session's raw uploaded bytes (the lossless JSONL the
// client sent, the source every projection is rebuilt from) to w in upload order,
// writing at most limit bytes. It returns the number of bytes written, whether the
// session held more than was written (so the caller can flag a truncated read), and
// the session's full raw length. A limit of zero or less means no cap. This is the
// raw underlying data behind the parsed transcript, exposed so an agent can inspect
// exactly what was ingested rather than only the projection. A missing session
// returns ErrNotFound.
func (s *Store) SessionRawTo(ctx context.Context, w io.Writer, sessionID, limit int64) (written int64, truncated bool, total int64, err error) {
	// total and the streamed content must come from one snapshot: an AppendChunk or
	// ResetRaw committing between the length read and the chunk stream would otherwise
	// let total_bytes describe one version of the raw while content is another. A
	// repeatable-read, read-only transaction pins both reads to the same MVCC snapshot,
	// so a concurrent writer is simply invisible to this reader rather than half-seen.
	txErr := pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly},
		func(tx pgx.Tx) error {
			// total is session_raw.byte_len, the running length AppendChunk and ResetRaw
			// maintain in the same transaction that writes the chunk rows (so the invariant
			// byte_len == sum(length(content)) holds at every committed state, pinned by
			// TestSessionRawByteLenMatchesChunks). Reading it is O(1) rather than scanning the
			// whole growing raw, and reading it inside this snapshot keeps it exactly
			// consistent with the chunks streamed below. A missing session_raw row (no upload
			// yet) is ErrNotFound.
			if err := tx.QueryRow(ctx,
				`SELECT byte_len FROM session_raw WHERE session_id = $1`, sessionID).Scan(&total); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrNotFound
				}
				return fmt.Errorf("read raw length for session %d: %w", sessionID, err)
			}

			rows, err := tx.Query(ctx,
				`SELECT byte_offset, byte_len, content
				   FROM session_raw_chunks WHERE session_id = $1 ORDER BY byte_offset`, sessionID)
			if err != nil {
				return fmt.Errorf("read raw chunks for session %d: %w", sessionID, err)
			}
			defer rows.Close()
			for rows.Next() {
				var off, length int64
				var content []byte
				if err := rows.Scan(&off, &length, &content); err != nil {
					return fmt.Errorf("scan raw chunk for session %d: %w", sessionID, err)
				}
				if limit > 0 && written+int64(len(content)) > limit {
					content = content[:limit-written]
					if _, err := w.Write(content); err != nil {
						return err
					}
					written += int64(len(content))
					truncated = true
					return nil
				}
				if _, err := w.Write(content); err != nil {
					return err
				}
				written += int64(len(content))
			}
			if err := rows.Err(); err != nil {
				return fmt.Errorf("iterate raw chunks for session %d: %w", sessionID, err)
			}
			truncated = written < total
			return nil
		})
	if txErr != nil {
		return written, truncated, total, txErr
	}
	return written, truncated, total, nil
}
