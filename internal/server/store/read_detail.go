package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jssblck/akari/internal/pricing"
	"github.com/jssblck/akari/internal/quality"
)

// scanDetailRow loads one session with its project into a SessionDetail by an arbitrary WHERE
// clause, also reporting whether its cache-savings rollup is backfilled. It is the single-query
// core that scanDetail wraps with the read-side backfill; every displayed field comes from this
// one sessions-row read, so the token split and the saving are always one consistent snapshot.
func (s *Store) scanDetailRow(ctx context.Context, where string, arg any) (SessionDetail, bool, error) {
	var d SessionDetail
	var cacheSavingsBackfilled bool
	err := s.Pool.QueryRow(ctx,
		`SELECT s.id, s.agent, s.machine, s.git_branch, u.username,
		        s.message_count, s.user_message_count, s.model_fallback_count,
		        s.total_input_tokens, s.total_output_tokens,
		        s.total_cache_write_tokens, s.total_cache_read_tokens,
		        s.total_cost_usd, s.cost_incomplete, s.visibility, s.public_id,
		        s.started_at, s.ended_at, s.last_active_at,
		        s.user_id, s.project_id, p.remote_key, p.display_name, p.kind, s.cwd, s.parent_session_id, s.parser_version,
		        s.total_cache_savings_usd, s.cache_savings_incomplete, s.cache_savings_backfilled,
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
		&d.OwnerID, &d.ProjectID, &d.ProjectKey, &d.ProjectName, &d.ProjectKind, &d.Cwd, &d.ParentID, &d.ParserVersion,
		&d.TotalCacheSavingsUSD, &d.CacheSavingsIncomplete, &cacheSavingsBackfilled,
		&d.Title)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionDetail{}, false, ErrNotFound
	}
	if err != nil {
		return SessionDetail{}, false, err
	}
	d.Title = squashSpaces(d.Title)
	return d, cacheSavingsBackfilled, nil
}

// scanDetail loads one session with its project, by an arbitrary WHERE clause.
//
// The Cache tile reads the total_cache_savings_usd rollup rather than rescanning usage_events per
// refresh, but that rollup is authoritative only once cache_savings_backfilled is set. A session
// that predates the column is seeded at 0 and left unbackfilled until it is priced, and the startup
// BackfillCacheSavings runs asynchronously, so a detail read can arrive before it. Serving the
// seeded value would put a wrong saving on the very session a reader opened. Two cheaper-looking
// fixes are both wrong: recomputing the saving on every read makes a live session under SSE pay a
// full usage_events scan per refresh, O(K^2) over its rows; and reading the token split from the
// sessions rollup while recomputing only the saving from usage_events lets a concurrent append tear
// the tile, pairing an old split with a newer saving. So this backfills the row once on demand,
// under the same locked primitive the startup pass uses: backfillCacheSavingsForSession prices the
// saving, persists it, and flips the flag (safe against the live parse fold), after which this read
// and every later one serve the O(1) rollup from one consistent scanDetailRow snapshot.
//
// A stored rollup is authoritative only when the corpus has been priced at THIS binary's rate table,
// which the singleton pricing marker records: cache_savings_priced_version == pricing.Version. When
// the marker differs, a pricing rollout is in flight in one of two directions, and either way a
// backfilled=true row may hold a saving at a different rate table than a live recompute would produce,
// so the rollup is provisional. A newer binary is ahead (marker > pricing.Version): the stored value
// was priced at the newer rates and this older binary must not present it as its own exact figure. Or
// this newer binary's own reconcile has not run yet (marker < pricing.Version): existing rows are
// still at the OLD rates while a live recompute here uses the new ones. In both cases the read serves
// the stored value flagged partial rather than pay a per-read recompute: this is the session-body SSE
// path, so an O(K) usage_events scan per refresh would be O(K^2) over a live session, the exact cost
// the total_cache_savings_usd rollup exists to avoid. The marker check is one O(1) singleton read, and
// the periodic reconcile re-prices the corpus to pricing.Version within a settle tick, so the partial
// flag clears itself; until then it reads as provisional (the tile appends "partial") rather than
// asserting a figure a recompute would contradict. In the steady state (marker current), that
// on-demand backfill described above is the whole story.
func (s *Store) scanDetail(ctx context.Context, where string, arg any) (SessionDetail, error) {
	marker, err := s.cacheSavingsPricedVersion(ctx)
	if err != nil {
		return SessionDetail{}, err
	}
	d, backfilled, err := s.scanDetailRow(ctx, where, arg)
	if err != nil {
		return SessionDetail{}, err
	}
	if marker != pricing.Version {
		// A pricing rollout is in flight (marker ahead or behind pricing.Version), so the stored rollup
		// may be at a different rate table than a live recompute. Serve it flagged partial, from the same
		// single row read as the token split so the two never tear, and do NOT recompute on this hot path.
		d.CacheSavingsIncomplete = true
		return d, nil
	}
	if backfilled {
		return d, nil
	}
	if _, err := s.backfillCacheSavingsForSession(ctx, d.ID); err != nil {
		return SessionDetail{}, fmt.Errorf("backfill cache savings for session %d on read: %w", d.ID, err)
	}
	// Re-read with the original predicate, not by id: the flag is now set (in the ordinary case), so
	// this returns the O(1) rollup from one post-backfill snapshot rather than a mix of the pre-backfill
	// scan and the new saving, and re-applying the same where clause rechecks any visibility gate (the
	// public path filters on visibility = 'public'), so a session unpublished between the two reads is
	// not rendered from the by-id re-read.
	d, backfilled, err = s.scanDetailRow(ctx, where, arg)
	if err != nil {
		return SessionDetail{}, err
	}
	if !backfilled {
		// The marker moved between the read above and the backfill (a concurrent reconcile winning the
		// marker), so backfillCacheSavingsForSession bowed out. Serve the stored value flagged partial for
		// the same reason as the marker-in-flight branch above rather than recompute on the SSE path.
		d.CacheSavingsIncomplete = true
	}
	return d, nil
}

// SessionDetailByID loads a session by numeric id.
func (s *Store) SessionDetailByID(ctx context.Context, id int64) (SessionDetail, error) {
	return s.scanDetail(ctx, "s.id = $1", id)
}

// SessionDetailByPublicID loads a published session by its public id.
func (s *Store) SessionDetailByPublicID(ctx context.Context, publicID string) (SessionDetail, error) {
	return s.scanDetail(ctx, "s.public_id = $1 AND s.visibility = 'public'", publicID)
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
// (duplicate_prompt on the messages row and the message_turn_usage rollup, both materialized at
// insert, see projection.go), not folded from whole-session windows here. So the live body fragment
// (handleSessionBody) re-fetching this on every SSE append reads bounded indexed rows and does no
// growing whole-session usage aggregation or message-window scan for either.
func (s *Store) Messages(ctx context.Context, sessionID int64) ([]Message, error) {
	return s.scanMessages(ctx, sessionID, messagesFullQuery, sessionID, quality.PromptFactsVersion)
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
		return s.scanMessages(ctx, sessionID,
			messagesWindowQuery+` ORDER BY m.ordinal LIMIT $3`,
			sessionID, quality.PromptFactsVersion, limit)
	}
	return s.scanMessages(ctx, sessionID,
		messagesWindowQuery+` AND m.ordinal > $3 ORDER BY m.ordinal LIMIT $4`,
		sessionID, quality.PromptFactsVersion, *after, limit)
}

// scanMessages runs a transcript read and scans its rows into Messages. sessionID is carried only
// to give the error path context: a cursor, network, or cancellation failure mid-read then names
// the session and the operation rather than surfacing a bare driver error to the handler.
func (s *Store) scanMessages(ctx context.Context, sessionID int64, query string, args ...any) ([]Message, error) {
	rows, err := s.Pool.Query(ctx, query, args...)
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

// ToolCalls returns all of a session's tool calls as metadata, for the web
// renderer. Bounded readers pass a message-ordinal range to ToolCallsInRange.
func (s *Store) ToolCalls(ctx context.Context, sessionID int64) ([]ToolCallView, error) {
	return s.scanToolCalls(ctx,
		`SELECT message_ordinal, call_index, tool_name, coalesce(category,''), coalesce(file_path,''), coalesce(file_rel_path,''), coalesce(detail,''),
		        coalesce(input_sha256,''), coalesce(input_bytes,0), coalesce(input_media_type,''),
		        coalesce(result_sha256,''), coalesce(result_bytes,0), coalesce(result_media_type,''), coalesce(result_status,'')
		   FROM tool_calls WHERE session_id = $1 ORDER BY message_ordinal, call_index`, sessionID)
}

// ToolCallsInRange returns the tool calls hanging on messages in the inclusive
// ordinal window [minOrdinal, maxOrdinal], so a bounded transcript read fetches
// only the calls for the messages it returned rather than the whole session.
func (s *Store) ToolCallsInRange(ctx context.Context, sessionID int64, minOrdinal, maxOrdinal int) ([]ToolCallView, error) {
	return s.scanToolCalls(ctx,
		`SELECT message_ordinal, call_index, tool_name, coalesce(category,''), coalesce(file_path,''), coalesce(file_rel_path,''), coalesce(detail,''),
		        coalesce(input_sha256,''), coalesce(input_bytes,0), coalesce(input_media_type,''),
		        coalesce(result_sha256,''), coalesce(result_bytes,0), coalesce(result_media_type,''), coalesce(result_status,'')
		   FROM tool_calls WHERE session_id = $1 AND message_ordinal BETWEEN $2 AND $3
		   ORDER BY message_ordinal, call_index`, sessionID, minOrdinal, maxOrdinal)
}

func (s *Store) scanToolCalls(ctx context.Context, query string, args ...any) ([]ToolCallView, error) {
	rows, err := s.Pool.Query(ctx, query, args...)
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
	return s.scanAttachments(ctx,
		`SELECT coalesce(message_ordinal, 0), sha256, coalesce(media_type,''), coalesce(byte_len,0), coalesce(filename,'')
		   FROM attachments WHERE session_id = $1 ORDER BY message_ordinal, id`, sessionID)
}

// AttachmentsInRange returns the attachments hanging on messages in the inclusive
// ordinal window [minOrdinal, maxOrdinal], so a bounded transcript read fetches
// only the attachments for the messages it returned.
func (s *Store) AttachmentsInRange(ctx context.Context, sessionID int64, minOrdinal, maxOrdinal int) ([]AttachmentView, error) {
	return s.scanAttachments(ctx,
		`SELECT coalesce(message_ordinal, 0), sha256, coalesce(media_type,''), coalesce(byte_len,0), coalesce(filename,'')
		   FROM attachments WHERE session_id = $1 AND message_ordinal BETWEEN $2 AND $3
		   ORDER BY message_ordinal, id`, sessionID, minOrdinal, maxOrdinal)
}

func (s *Store) scanAttachments(ctx context.Context, query string, args ...any) ([]AttachmentView, error) {
	rows, err := s.Pool.Query(ctx, query, args...)
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

// Subagents returns sessions whose parent is the given session.
func (s *Store) Subagents(ctx context.Context, parentID int64) ([]SessionSummary, error) {
	rows, err := s.Pool.Query(ctx, sessionSelect+" WHERE s.parent_session_id = $1 ORDER BY s.id", parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionSummary
	for rows.Next() {
		sm, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}
