package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// ProjToolCall is one tool_calls insert. The input body lives in the CAS, by one
// of two paths: InputBody holds the bulky input inline and AdvanceProjection
// writes it and records the sha256; or InputSHA256 is already set because the
// client lifted the body to the CAS at upload time and left a sentinel, so the
// reference is recorded with no blob write. Exactly one of InputBody / InputSHA256
// is set when there is an input. CallUID is the agent's call id, used to
// back-patch the result that arrives on a later line (and possibly a later
// region, for Claude).
type ProjToolCall struct {
	MessageOrdinal int
	CallIndex      int
	ToolName       string
	Category       string
	FilePath       string
	InputBody      string
	InputSHA256    string
	InputBytes     int64
	InputMediaType string
	CallUID        string
}

// ToolResultDelta back-patches a tool call's result, matched by call id. The
// result body reaches the CAS by one of two paths, mirroring ProjToolCall: Body
// holds it inline for the server to write, or BodySHA256 is the reference the
// client already uploaded. Both are empty when the result carries no body.
type ToolResultDelta struct {
	CallUID    string
	Body       string
	BodySHA256 string
	Bytes      int64
	MediaType  string
	Status     string
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

// AttachmentDelta is one attachments insert (today a lifted image). Like a tool
// body it reaches the CAS by one of two paths: when the client lifted the image,
// SHA256 names the already-uploaded blob and applyDelta records the reference with no
// blob write; otherwise Body holds the decoded bytes inline for the server to store.
// Bytes and MediaType describe the decoded image so the row carries its size and type
// without fetching the blob.
type AttachmentDelta struct {
	MessageOrdinal int
	SHA256         string
	Body           string
	Bytes          int64
	MediaType      string
	Filename       string
}

// ProjectionDelta is the incremental projection write for one parsed region: the
// rows to add and the region's timestamp span. The session rollups are not folded
// from precomputed counters carried here. They are derived from the rows that
// actually persist (see appliedDelta), because the row inserts dedup on conflict
// and the rollups must count exactly the surviving set. Claude repeats a usage
// block across sidechain and summary lines, so a region can carry the same usage
// several times while the ledger keeps one; folding precomputed per-region deltas
// over-counted those duplicates.
type ProjectionDelta struct {
	Messages    []MessageDelta
	ToolCalls   []ProjToolCall
	ToolResults []ToolResultDelta
	Usage       []ProjUsage
	Attachments []AttachmentDelta

	Started time.Time
	Ended   time.Time
}

// appliedDelta is what one region's writes actually persisted: the rows that
// inserted rather than the rows the reducer proposed. The ON CONFLICT DO NOTHING
// guards on messages and usage drop replays and Claude's duplicated usage blocks,
// so only the inserted rows contribute here. Folding this into the session rollups
// is what holds the invariant that, for every agent, sessions.total_* equals the
// matching sum over usage_events and message_count equals the count of messages
// rows. Tool calls and results carry no rollup column, so they do not appear here.
type appliedDelta struct {
	MessagesAdded     int
	UserMessagesAdded int
	Input             int64
	Output            int64
	CacheWrite        int64
	CacheRead         int64
	CostUSD           float64
	CostIncomplete    bool
}

// roleUser is the message role that counts toward user_message_count. The reducer
// emits it as the normalized parser.RoleUser string; the store compares the stored
// string so it does not depend on the parser package.
const roleUser = "user"

// ReduceFunc parses a raw region beginning at baseOffset, given the prior
// serialized parser state, and returns the new state plus the projection delta.
// It is pure CPU: AdvanceProjection runs it inside the parse transaction, so it
// must not perform I/O.
type ReduceFunc func(state, region []byte, baseOffset int64) (newState []byte, d ProjectionDelta, err error)

// ReparseTarget identifies a session to re-parse. The reparse loop fetches targets
// a bounded page at a time (see SessionsForReparsePage), so the server never holds
// the whole session list resident at once.
type ReparseTarget struct {
	ID    int64
	Agent string
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
		applied, err := applyDelta(ctx, tx, sessionID, d)
		if err != nil {
			return err
		}

		if _, err := tx.Exec(ctx,
			`UPDATE session_raw
			    SET parsed_byte_len = $2, parse_state = $3, parse_state_version = $4, parse_error = ''
			  WHERE session_id = $1`,
			sessionID, regionEnd, newState, parserVersion); err != nil {
			return fmt.Errorf("advance parse cursor for session %d to %d: %w", sessionID, regionEnd, err)
		}
		if err := applyAggregates(ctx, tx, sessionID, parserVersion, applied, d.Started, d.Ended); err != nil {
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
		_, err := applyDelta(ctx, tx, sessionID, d)
		return err
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

// applyDelta writes one region's rows (message upserts, tool-call inserts with
// their input bodies in the CAS, tool-result back-patches, and usage inserts) and
// returns the aggregates that actually persisted. Each insert that survives its
// ON CONFLICT guard contributes to the returned appliedDelta; a row dropped as a
// duplicate does not, so the caller folds the deduped set into the session rollups
// rather than the reducer's pre-dedup proposal.
func applyDelta(ctx context.Context, tx pgx.Tx, sessionID int64, d ProjectionDelta) (appliedDelta, error) {
	var applied appliedDelta

	// Each ordinal is inserted once: a turn is folded whole within the region that
	// carries it, so there is no in-place content rewrite and no quadratic append.
	// The ON CONFLICT DO NOTHING is a replay guard (a region is parsed once, since
	// the cursor advances in the same transaction, and a reparse deletes these rows
	// first), so a retried region never duplicates or rewrites a row. Counting only
	// rows that inserted keeps message_count equal to the count of messages rows
	// even if a region is ever replayed.
	for _, m := range d.Messages {
		tag, err := tx.Exec(ctx,
			`INSERT INTO messages
			   (session_id, ordinal, role, content, thinking_text, model, timestamp, has_thinking, has_tool_use)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			 ON CONFLICT (session_id, ordinal) DO NOTHING`,
			sessionID, m.Ordinal, sanitizeText(m.Role), sanitizeText(m.Content),
			sanitizeText(m.ThinkingText), sanitizeText(m.Model),
			nullTime(m.Timestamp), m.HasThinking, m.HasToolUse)
		if err != nil {
			return appliedDelta{}, fmt.Errorf("write message %d for session %d: %w", m.Ordinal, sessionID, err)
		}
		if tag.RowsAffected() > 0 {
			applied.MessagesAdded++
			if m.Role == roleUser {
				applied.UserMessagesAdded++
			}
		}
	}

	for _, t := range d.ToolCalls {
		var inputSHA, inputMedia any
		switch {
		case t.InputSHA256 != "":
			// The client lifted the input to the CAS and left a sentinel; the blob is
			// already present (and pinned against the sweep), so record the reference
			// without re-storing the body. Re-lock it FOR KEY SHARE so a sweep racing
			// this insert cannot delete the blob between here and the FK check.
			if err := pinBlobRefTx(ctx, tx, t.InputSHA256); err != nil {
				return appliedDelta{}, fmt.Errorf("reference tool input blob %s for session %d call %d/%d: %w", t.InputSHA256, sessionID, t.MessageOrdinal, t.CallIndex, err)
			}
			inputSHA, inputMedia = t.InputSHA256, sanitizeText(t.InputMediaType)
		case len(t.InputBody) > 0:
			sha, err := writeBlobTx(ctx, tx, t.InputBody, t.InputMediaType)
			if err != nil {
				return appliedDelta{}, fmt.Errorf("write tool input blob for session %d call %d/%d: %w", sessionID, t.MessageOrdinal, t.CallIndex, err)
			}
			inputSHA, inputMedia = sha, sanitizeText(t.InputMediaType)
		}
		// call_uid is the agent's own tool_use id, and the tool-result back-patch keys
		// on it. The (session_id, call_uid) index is deliberately non-unique (migration
		// 0010): a resumed or compacted Claude transcript replays prior assistant turns
		// verbatim, so the same id legitimately rides more than one row, and under a
		// unique index the second insert tripped the constraint and rolled back the whole
		// parse (the reparse failure on four sessions). Storing the id on every row lets
		// the back-patch UPDATE ... WHERE call_uid = $1 stamp the same result onto each
		// replayed copy, which is what a reader expects to see. The ON CONFLICT still
		// guards the (message_ordinal, call_index) key against a region replay. A session
		// that carries a duplicate id is surfaced in the UI (DuplicateCallUIDCount), so a
		// genuinely malformed id reuse, the only case where stamping both rows is wrong,
		// is visible rather than silent.
		if _, err := tx.Exec(ctx,
			`INSERT INTO tool_calls
			   (session_id, message_ordinal, call_index, tool_name, category, file_path,
			    input_sha256, input_bytes, input_media_type, call_uid)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			 ON CONFLICT (session_id, message_ordinal, call_index) DO NOTHING`,
			sessionID, t.MessageOrdinal, t.CallIndex, sanitizeText(t.ToolName), sanitizeText(t.Category),
			nullString(sanitizeText(t.FilePath)),
			inputSHA, t.InputBytes, inputMedia, nullString(sanitizeText(t.CallUID))); err != nil {
			return appliedDelta{}, fmt.Errorf("insert tool call %d/%d for session %d: %w", t.MessageOrdinal, t.CallIndex, sessionID, err)
		}
	}

	for _, tr := range d.ToolResults {
		if tr.CallUID == "" {
			continue
		}
		var resultSHA any
		switch {
		case tr.BodySHA256 != "":
			if err := pinBlobRefTx(ctx, tx, tr.BodySHA256); err != nil {
				return appliedDelta{}, fmt.Errorf("reference tool result blob %s for session %d call %q: %w", tr.BodySHA256, sessionID, tr.CallUID, err)
			}
			resultSHA = tr.BodySHA256
		case len(tr.Body) > 0:
			sha, err := writeBlobTx(ctx, tx, tr.Body, tr.MediaType)
			if err != nil {
				return appliedDelta{}, fmt.Errorf("write tool result blob for session %d call %q: %w", sessionID, tr.CallUID, err)
			}
			resultSHA = sha
		}
		media := sanitizeText(tr.MediaType)
		if media == "" {
			media = "text/plain"
		}
		// Patches one row in the common case and every still-pending copy when a
		// transcript repeated the call's id (the index is non-unique by design), so each
		// visible copy of a duplicated turn carries the same result rather than one
		// looking pending. The result_status IS NULL predicate both makes the write
		// once-per-row and lets the pending-only partial index (idx_tool_calls_pending_result,
		// migration 0011) serve the lookup: a row leaves that index when its result lands,
		// so a replayed turn that delivers its tool_result K times probes only the copies
		// still pending rather than re-scanning all K accumulated rows each time. That
		// keeps the back-patch linear in the number of rows instead of O(K^2).
		if _, err := tx.Exec(ctx,
			`UPDATE tool_calls
			    SET result_sha256 = $3, result_bytes = $4, result_media_type = $5, result_status = $6
			  WHERE session_id = $1 AND call_uid = $2 AND result_status IS NULL`,
			sessionID, sanitizeText(tr.CallUID), resultSHA, tr.Bytes, media, sanitizeText(tr.Status)); err != nil {
			return appliedDelta{}, fmt.Errorf("back-patch tool result for session %d call %q: %w", sessionID, tr.CallUID, err)
		}
	}

	// Only usage rows that actually insert fold into the rollups. Claude repeats a
	// usage block across sidechain and summary lines (same dedup_key), and Codex's
	// replays collide on (source_offset, source_index); ON CONFLICT DO NOTHING keeps
	// one in the ledger, and counting RowsAffected here keeps the rollup in lockstep
	// with that surviving set. cost_incomplete is derived the same way: a surviving
	// row that carries tokens but no priced cost is what makes the session total a
	// partial sum.
	for _, u := range d.Usage {
		var ord, cost any
		if u.MessageOrdinal != nil {
			ord = *u.MessageOrdinal
		}
		if u.CostUSD != nil {
			cost = *u.CostUSD
		}
		tag, err := tx.Exec(ctx,
			`INSERT INTO usage_events
			   (session_id, message_ordinal, model, input_tokens, output_tokens,
			    cache_write_tokens, cache_read_tokens, reasoning_tokens, cost_usd,
			    occurred_at, dedup_key, source_offset, source_index)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			 ON CONFLICT DO NOTHING`,
			sessionID, ord, sanitizeText(u.Model), u.Input, u.Output, u.CacheWrite, u.CacheRead,
			u.Reasoning, cost, nullTime(u.OccurredAt), sanitizeText(u.DedupKey), u.SourceOffset, u.SourceIndex)
		if err != nil {
			return appliedDelta{}, fmt.Errorf("insert usage event for session %d at offset %d: %w", sessionID, u.SourceOffset, err)
		}
		if tag.RowsAffected() == 0 {
			continue
		}
		applied.Input += int64(u.Input)
		applied.Output += int64(u.Output)
		applied.CacheWrite += int64(u.CacheWrite)
		applied.CacheRead += int64(u.CacheRead)
		switch {
		case u.CostUSD != nil:
			applied.CostUSD += *u.CostUSD
		case u.Input+u.Output+u.CacheWrite+u.CacheRead+u.Reasoning > 0:
			// Tokens spent on a model the pricing table does not know: the session
			// total is a partial sum and the flag says so.
			applied.CostIncomplete = true
		}
	}
	// Attachments carry no rollup column, so they do not fold into appliedDelta; they
	// are inserted here for their blob references and metadata. Like a tool body each
	// reaches the CAS by one of two paths: a client-lifted image names an already
	// uploaded blob (record the reference, re-locking it FOR KEY SHARE so a racing sweep
	// cannot reclaim it before the FK is checked), and an inline image is written here.
	for _, a := range d.Attachments {
		var sha any
		switch {
		case a.SHA256 != "":
			if err := pinBlobRefTx(ctx, tx, a.SHA256); err != nil {
				return appliedDelta{}, fmt.Errorf("reference attachment blob %s for session %d ordinal %d: %w", a.SHA256, sessionID, a.MessageOrdinal, err)
			}
			sha = a.SHA256
		case len(a.Body) > 0:
			s, err := writeBlobTx(ctx, tx, a.Body, a.MediaType)
			if err != nil {
				return appliedDelta{}, fmt.Errorf("write attachment blob for session %d ordinal %d: %w", sessionID, a.MessageOrdinal, err)
			}
			sha = s
		default:
			continue // an attachment with no body is nothing to store
		}
		media := sanitizeText(a.MediaType)
		if media == "" {
			media = "application/octet-stream"
		}
		// The unique index (session_id, message_ordinal, sha256) makes a replayed region
		// a no-op, so a retried chunk never double-inserts an attachment.
		if _, err := tx.Exec(ctx,
			`INSERT INTO attachments (session_id, message_ordinal, sha256, filename, media_type, byte_len)
			 VALUES ($1,$2,$3,$4,$5,$6)
			 ON CONFLICT (session_id, message_ordinal, sha256) DO NOTHING`,
			sessionID, a.MessageOrdinal, sha, nullString(sanitizeText(a.Filename)), media, a.Bytes); err != nil {
			return appliedDelta{}, fmt.Errorf("insert attachment for session %d ordinal %d: %w", sessionID, a.MessageOrdinal, err)
		}
	}
	return applied, nil
}

// applyAggregates folds a region's persisted increments into the session rollups.
// The counts and token/cost totals come from appliedDelta, the rows that actually
// inserted, not from a pre-dedup per-region count, so the rollups equal the ledger
// (sessions.total_* == sum over usage_events, message_count == count of messages
// rows) for every agent. The span widens by LEAST/GREATEST (both ignore NULLs, so
// a region with no timestamps leaves the bounds unchanged); cost_incomplete is
// sticky once any surviving unpriced usage row is seen.
func applyAggregates(ctx context.Context, tx pgx.Tx, sessionID int64, parserVersion int, a appliedDelta, started, ended time.Time) error {
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
		sessionID, a.MessagesAdded, a.UserMessagesAdded,
		a.Input, a.Output, a.CacheWrite, a.CacheRead,
		a.CostUSD, a.CostIncomplete,
		nullTime(started), nullTime(ended), parserVersion)
	if err != nil {
		return fmt.Errorf("update aggregates for session %d: %w", sessionID, err)
	}
	return nil
}

// ResetProjectionForReparse clears a session's parser-owned projection rows and its
// aggregates, and rewinds the parse cursor to zero at the given version, keeping the
// raw bytes and their hash. It is the standalone clear without the replay: the server
// reparses through ReparseSession, which composes this clear with an in-transaction
// replay so the rebuild is atomic. This form is kept for store tests that exercise the
// clear and its blob pin on their own. Attachments are parser-owned (the reducer emits
// them from the transcript's image events), so they are cleared here too; a reparse
// rewrites them, and the orphan sweep reclaims any blob left unreferenced.
func (s *Store) ResetProjectionForReparse(ctx context.Context, sessionID int64, parserVersion int) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Parent session first, then session_raw: the same order DeleteSession takes,
		// so a concurrent delete cannot deadlock with a reparse.
		if err := lockSession(ctx, tx, sessionID); err != nil {
			return err
		}
		if _, err := lockSessionRaw(ctx, tx, sessionID); err != nil {
			return err
		}
		return clearProjectionForReparseTx(ctx, tx, sessionID, parserVersion)
	})
}

// ReparseSession rebuilds a session's projection from its stored raw bytes in a
// single transaction: it clears the derived rows and rewinds the cursor, then
// replays the whole session through reduce, all atomically. Because the clear and
// the replay commit together, two correctness properties hold that the older
// reset-then-advance path did not provide:
//
//   - A concurrent reader never sees the session empty or half rebuilt. It sees the
//     prior projection until this transaction commits, then the new one; there is no
//     window of cleared-but-not-yet-replayed rows.
//   - Any failure rolls the whole rebuild back. A parser error on malformed bytes or
//     an operational store/CAS error leaves the prior projection intact rather than a
//     cleared session, so a per-session parser failure loses no data and an
//     operational failure is safe to retry.
//
// The raw bytes are never modified. The replay is bounded to one region at a time
// (parseBatchBytes), so peak memory does not scale with session size even though the
// whole session is rebuilt in one transaction.
func (s *Store) ReparseSession(ctx context.Context, sessionID int64, parserVersion int, reduce ReduceFunc) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := lockSession(ctx, tx, sessionID); err != nil {
			return err
		}
		byteLen, err := lockSessionRaw(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if err := clearProjectionForReparseTx(ctx, tx, sessionID, parserVersion); err != nil {
			return err
		}
		// Replay every region from the rewound cursor in this same transaction.
		// readRawRegion guarantees the chunk at the cursor always qualifies, so the
		// loop makes progress and ends exactly on the stored length.
		state := []byte("{}")
		var parsedLen int64
		for parsedLen < byteLen {
			region, regionEnd, err := readRawRegion(ctx, tx, sessionID, parsedLen, parseBatchBytes)
			if err != nil {
				return err
			}
			newState, d, err := reduce(state, region, parsedLen)
			if err != nil {
				return fmt.Errorf("parse session %d region [%d,%d): %w", sessionID, parsedLen, regionEnd, err)
			}
			applied, err := applyDelta(ctx, tx, sessionID, d)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`UPDATE session_raw
				    SET parsed_byte_len = $2, parse_state = $3, parse_state_version = $4, parse_error = ''
				  WHERE session_id = $1`,
				sessionID, regionEnd, newState, parserVersion); err != nil {
				return fmt.Errorf("advance parse cursor for session %d to %d: %w", sessionID, regionEnd, err)
			}
			if err := applyAggregates(ctx, tx, sessionID, parserVersion, applied, d.Started, d.Ended); err != nil {
				return err
			}
			state = newState
			parsedLen = regionEnd
		}
		return nil
	})
}

// lockSessionRaw locks the session_raw row (the caller must already hold the parent
// session row, per the (session, session_raw) order DeleteSession takes) and returns
// the stored byte length. It centralizes the FOR UPDATE the reparse and reset paths
// share.
func lockSessionRaw(ctx context.Context, tx pgx.Tx, sessionID int64) (int64, error) {
	var byteLen int64
	if err := tx.QueryRow(ctx,
		`SELECT byte_len FROM session_raw WHERE session_id = $1 FOR UPDATE`, sessionID).Scan(&byteLen); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, fmt.Errorf("lock session_raw for session %d: %w", sessionID, err)
	}
	return byteLen, nil
}

// clearProjectionForReparseTx clears the parser-owned rows, pins the session's blobs,
// rewinds the cursor to zero at parserVersion, and zeroes the aggregates, within the
// caller's transaction (which must already hold the session and session_raw locks).
// It is shared by ReparseSession and the standalone ResetProjectionForReparse.
func clearProjectionForReparseTx(ctx context.Context, tx pgx.Tx, sessionID int64, parserVersion int) error {
	// Pin every blob this session references (lifted tool inputs and results, and
	// image attachments) before clearing the rows that reference them, so a concurrent
	// sweep cannot reclaim a still-live blob. In ReparseSession the clear and rebuild
	// commit together so this is belt-and-suspenders; in the standalone reset it is
	// load-bearing for the window before a separate rebuild re-records the reference.
	// The pin (FOR KEY SHARE via the FK) conflicts with the sweep's FOR UPDATE.
	if err := pinSessionBlobsTx(ctx, tx, sessionID); err != nil {
		return err
	}
	for _, q := range []string{
		"DELETE FROM messages WHERE session_id = $1",
		"DELETE FROM tool_calls WHERE session_id = $1",
		"DELETE FROM usage_events WHERE session_id = $1",
		"DELETE FROM attachments WHERE session_id = $1",
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

// sanitizeText makes a session-derived string safe for a Postgres text column.
// Postgres rejects a NUL byte (0x00) outright and any byte sequence that is not
// valid UTF-8, and a single offending byte fails the whole INSERT: that is how a
// reparse of one Claude session (a message body carrying a raw NUL) rolled back
// and kept its stale projection. Replacing the bad bytes with U+FFFD keeps the
// row writable and marks where the transcript was malformed, rather than dropping
// the message or stranding the session. NUL is itself valid UTF-8, so ToValidUTF8
// leaves it in place and it is replaced separately; both calls return the input
// unchanged when there is nothing to fix, so the clean path does not allocate.
func sanitizeText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "�")
}
