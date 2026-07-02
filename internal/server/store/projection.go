package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jssblck/akari/internal/pricing"
	"github.com/jssblck/akari/internal/quality"
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
// and the rollups must count exactly the surviving set. Claude streams one
// assistant message across several transcript lines that share its message id, so a
// region can carry the same usage block several times while the ledger keeps one;
// folding precomputed per-region deltas over-counted those duplicates.
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
	// CacheSavingsUSD is the prompt-cache dollars the surviving rows saved versus paying
	// the uncached input rate for the same cached volume, priced per model (the rate gap
	// differs by family and is negative on a Claude cache write). It folds into the session
	// rollup like CostUSD so the session header's Cache tile reads one row rather than
	// rescanning usage_events on every live refresh. CacheSavingsIncomplete is sticky like
	// CostIncomplete: set when a surviving row carried cached volume on an unpriced model,
	// so that row's saving is omitted. Unlike cost it is not a clean lower bound (the
	// omitted term can be either sign), which the UI reflects as "partial" rather than "+".
	CacheSavingsUSD        float64
	CacheSavingsIncomplete bool
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
		// The append path deliberately does NOT refresh signals. Signals read the whole
		// session (the last word, failure streaks across the transcript, the per-turn
		// context sequence), so refreshing them here would recompute the entire session on
		// every caught-up append, turning a live session's K appends into O(K^2) ingest
		// work. It would also bake a time-dependent verdict into the row: the abandoned
		// versus unknown outcome depends on how long the session has been idle, so a refresh
		// taken mid-session stores a verdict that drifts once the session crosses the idle
		// threshold. Both are avoided by computing signals once, after the session settles,
		// off the ingest path: RefreshSettledSignals (a periodic pass) refreshes a session
		// only once it has been idle past the abandoned threshold, so the append path stays
		// linear and the stored outcome is computed when it is stable. A reparse still
		// refreshes at the end of its full replay (that is the versioned backfill, not the
		// append path).
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
		content := sanitizeText(m.Content)
		// Derive the prompt's hygiene facts here, where the body is already resident, and store
		// them beside the row as fixed-size columns. The settle pass then aggregates those columns
		// instead of reading every prompt body back to re-classify it, so its peak memory does not
		// track the largest prompt a session held (see quality.ClassifyPrompt and gatherPromptHygiene).
		// Only real human turns carry facts; other rows leave the columns NULL and the hygiene
		// aggregate reads role='user' only. The facts are stamped with quality.PromptFactsVersion so a
		// later change to the classifier is told apart from the current rules: the settle pass treats an
		// old-version row like an unfilled one until the reparse re-derives it (see gatherPromptHygiene).
		var pShort, pNoCode, pGreeting, pDigest, pFactsVersion any
		if m.Role == roleUser {
			facts := quality.ClassifyPrompt(content)
			pShort, pNoCode, pGreeting, pDigest = facts.Short, facts.NoCodeContext, facts.BareGreeting, facts.Digest
			pFactsVersion = quality.PromptFactsVersion
		}
		tag, err := tx.Exec(ctx,
			`INSERT INTO messages
			   (session_id, ordinal, role, content, thinking_text, model, timestamp, has_thinking, has_tool_use,
			    prompt_short, prompt_no_code, prompt_bare_greeting, prompt_digest, prompt_facts_version)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			 ON CONFLICT (session_id, ordinal) DO NOTHING`,
			sessionID, m.Ordinal, sanitizeText(m.Role), content,
			sanitizeText(m.ThinkingText), sanitizeText(m.Model),
			nullTime(m.Timestamp), m.HasThinking, m.HasToolUse,
			pShort, pNoCode, pGreeting, pDigest, pFactsVersion)
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

	// Load the session's cwd once for the whole delta, not per tool call, so deriving each
	// call's worktree-invariant relative path (see below) costs one SELECT per region rather
	// than one per edit. It is empty when the session announced no cwd, which sessionRelPath
	// treats as "no anchor" and leaves the relative path NULL for absolute paths.
	var sessionCwd string
	if len(d.ToolCalls) > 0 {
		if err := tx.QueryRow(ctx, `SELECT COALESCE(cwd, '') FROM sessions WHERE id = $1`, sessionID).Scan(&sessionCwd); err != nil {
			return appliedDelta{}, fmt.Errorf("load cwd for session %d: %w", sessionID, err)
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
		// Store the session-relative form of the path beside the absolute one. file_path is
		// absolute, so the same repo file edited from two worktrees of one repo fragments into
		// separate churn rows; file_rel_path is the worktree-invariant key that (paired with the
		// project, which already collapses worktrees on the canonical remote) collapses them back
		// together. The projection is the one place that sees both the session's cwd and the parsed
		// path, so it is where the two are reconciled. It is NULL when no stable relative form
		// exists (a path outside the workspace, or an absolute path with no announced cwd), which
		// churn coalesces back onto file_path so an unanchored edit still counts under its absolute
		// name rather than vanishing.
		var relPath any
		if rel, ok := sessionRelPath(sessionCwd, sanitizeText(t.FilePath)); ok {
			relPath = rel
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO tool_calls
			   (session_id, message_ordinal, call_index, tool_name, category, file_path, file_rel_path,
			    input_sha256, input_bytes, input_media_type, call_uid)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
			 ON CONFLICT (session_id, message_ordinal, call_index) DO NOTHING`,
			sessionID, t.MessageOrdinal, t.CallIndex, sanitizeText(t.ToolName), sanitizeText(t.Category),
			nullString(sanitizeText(t.FilePath)), relPath,
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

	// Only usage rows that actually insert fold into the rollups. Claude streams one
	// assistant message across several transcript lines that share a dedup_key, and
	// Codex's replays collide on (source_offset, source_index); ON CONFLICT DO NOTHING
	// keeps one in the ledger, and counting RowsAffected here keeps the rollup in
	// lockstep with that surviving set. cost_incomplete is derived the same way: a
	// surviving row that carries tokens but no priced cost is what makes the session
	// total a partial sum.
	//
	// DO NOTHING (not DO UPDATE) is deliberate: a DO UPDATE would fold a duplicate's
	// tokens into the rollup (its RowsAffected is 1 too), breaking the
	// sessions.total_* == sum(usage_events) invariant.
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
		// Fold the prompt-cache saving the same way, over the same surviving rows. Pricing
		// is linear in tokens, so summing each row's saving equals summing the model's
		// grouped totals (what SessionCacheStats does over the whole session), which is what
		// lets the rollup and that per-model recompute reconcile exactly. Cost is a stored
		// per-row figure; the saving is not, so it is priced here rather than read off the row.
		if saving, ok := pricing.CacheSavings(u.Model, int64(u.CacheRead), int64(u.CacheWrite)); ok {
			applied.CacheSavingsUSD += saving
		} else if u.CacheRead > 0 || u.CacheWrite > 0 {
			applied.CacheSavingsIncomplete = true
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
// a region with no timestamps leaves the bounds unchanged); cost_incomplete and
// cache_savings_incomplete are both sticky once any surviving unpriced usage row is
// seen. total_cache_savings_usd folds like total_cost_usd, so the session header's Cache
// tile reads the saving off this one row rather than rescanning usage_events per refresh.
//
// The one wrinkle is a pricing rolling deploy. This fold prices the region's saving with the running
// binary's rate table (CacheSavings, in applyDelta), so a fold is consistent with the stored rollup
// only when the corpus is priced at that same table, which the singleton marker records as
// cache_savings_priced_version == pricing.Version. When the marker differs, in either direction, a fold
// onto a backfilled=true row would mix rate tables and leave the row flagged authoritative. Marker
// ahead of this binary: a newer binary priced the corpus at newer rates, so this older binary's fold
// would splice an old-rate saving in. Marker behind: this newer binary's reconcile has not run yet, so
// the row is still at the OLD rates while this fold adds a new-rate saving. So when this region carried
// cache volume and the marker is not equal to pricing.Version, the fold drops cache_savings_backfilled
// to false, returning the session to the backfill candidate set for the reconcile and drain to re-price
// the whole saving from usage_events at the settled rate table. In steady state (marker current) the
// flag is untouched and the O(1) rollup stays authoritative.
func applyAggregates(ctx context.Context, tx pgx.Tx, sessionID int64, parserVersion int, a appliedDelta, started, ended time.Time) error {
	regionHasCache := a.CacheRead > 0 || a.CacheWrite > 0
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
		   total_cache_savings_usd = total_cache_savings_usd + $10,
		   cache_savings_incomplete = cache_savings_incomplete OR $11,
		   -- Drop the authoritative flag when this cache-bearing region was folded while the corpus
		   -- pricing marker differs from this binary's pricing.Version (a rollout in either direction),
		   -- so the reconcile and drain re-price the whole saving at the settled rate table. The COALESCE
		   -- reads a missing singleton as 0, which differs from any real version, so the safe direction
		   -- (treat as in-flight, drop the flag) also covers a pre-migration database.
		   cache_savings_backfilled = cache_savings_backfilled
		     AND NOT ($15 AND COALESCE((SELECT cache_savings_priced_version FROM parse_meta WHERE id = TRUE), 0) <> $16),
		   started_at = LEAST(started_at, $12),
		   ended_at = GREATEST(ended_at, $13),
		   parser_version = $14,
		   updated_at = now(),
		   -- The projection moved, so any stored grade is now behind it. Mark the session for
		   -- the settle pass to re-grade; refreshSignalsTx clears this once it grades a settled
		   -- session (see signals.go). This is what lets the settle pass find due sessions by an
		   -- index seek instead of rescanning the settled tail every wake.
		   signals_stale = true
		 WHERE id = $1`,
		sessionID, a.MessagesAdded, a.UserMessagesAdded,
		a.Input, a.Output, a.CacheWrite, a.CacheRead,
		a.CostUSD, a.CostIncomplete,
		a.CacheSavingsUSD, a.CacheSavingsIncomplete,
		nullTime(started), nullTime(ended), parserVersion,
		regionHasCache, pricing.Version)
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
		// The projection is fully rebuilt; recompute the session's signals from it in
		// this same transaction. A reparse is also the versioned backfill: a caught-up
		// session never re-enters AdvanceProjection, so an Epoch bump that reparses the
		// corpus is what fills signals for sessions ingested before they existed, and
		// what re-grades every session when the scoring version changes.
		return refreshSignalsTx(ctx, tx, sessionID)
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
		// session_signals is parser-owned too (derived from messages and tool_calls), so
		// it clears with the rest. ReparseSession rebuilds it at the end of its replay;
		// the standalone reset leaves it absent until the settle pass refreshes it, once
		// the re-parsed session has settled (the append path that catches the cursor back
		// up no longer touches signals, so the row does not come back on catch-up).
		"DELETE FROM session_signals WHERE session_id = $1",
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
		   total_cache_savings_usd = 0, cache_savings_incomplete = FALSE,
		   started_at = NULL, ended_at = NULL,
		   updated_at = now(),
		   -- Clearing the projection moves it, so the stored grade is behind. The reparse
		   -- replays and then refreshSignalsTx re-settles this flag (false only if the rebuilt
		   -- session is settled), so a reparse of a still-live session leaves it due.
		   signals_stale = true
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
