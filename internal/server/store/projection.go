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

// rebuildRegionBytes bounds how much raw content one readRawRegion call holds
// resident while feeding the reducer, so streaming a large session's bytes into
// the fold never concatenates the whole transcript into one buffer. The fold's
// own output (the ProjectionDelta) is whole-session by design; this bounds only
// the raw side. It is a var so tests can shrink it to force multi-region feeds.
var rebuildRegionBytes int64 = 16 << 20

// MessageDelta is one message row. Each ordinal appears exactly once: the
// reducer folds a whole session in one pass, so Content and ThinkingText are the
// complete text of the turn. ThinkingBytes is the reasoning-trace weight the
// observed-thinking signal sums (see parser.Message.ThinkingBytes): the
// plaintext length where the agent logs it, else the encrypted payload length,
// so a redacted turn still records its volume.
type MessageDelta struct {
	Ordinal       int
	Role          string
	Content       string
	ThinkingText  string
	ThinkingBytes int
	Model         string
	HasThinking   bool
	HasToolUse    bool
	Timestamp     time.Time
}

// ProjToolCall is one tool_calls row. The input body lives in the CAS, by one
// of two paths: InputBody holds the bulky input inline and the rebuild writes it
// and records the sha256; or InputSHA256 is already set because the client
// lifted the body to the CAS at upload time and left a sentinel, so the
// reference is recorded with no blob write. Exactly one of InputBody /
// InputSHA256 is set when there is an input. CallUID is the agent's call id,
// which the fold uses to patch the result that arrives on a later line. Detail
// is the bounded human-scannable summary of the input (a command, pattern, URL,
// or description) the UI shows when a call has no file_path; it is empty when
// the input has no summarizable key or was lifted before the field existed.
type ProjToolCall struct {
	MessageOrdinal int
	CallIndex      int
	ToolName       string
	Category       string
	FilePath       string
	Detail         string
	InputBody      string
	InputSHA256    string
	InputBytes     int64
	InputMediaType string
	CallUID        string
}

// ToolResultDelta patches a tool call's result, matched by call id. The result
// body reaches the CAS by one of two paths, mirroring ProjToolCall: Body holds
// it inline for the server to write, or BodySHA256 is the reference the client
// already uploaded. Both are empty when the result carries no body.
type ToolResultDelta struct {
	CallUID    string
	Body       string
	BodySHA256 string
	Bytes      int64
	MediaType  string
	Status     string
}

// ProjUsage is one usage event as the reducer saw it, pre-dedup. SourceOffset
// and SourceIndex identify the transcript line it came from; together with
// DedupKey they drive the in-memory dedup (see foldUsage).
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

// AttachmentDelta is one attachments row (today a lifted image). Like a tool
// body it reaches the CAS by one of two paths: when the client lifted the image,
// SHA256 names the already-uploaded blob and the rebuild records the reference
// with no blob write; otherwise Body holds the decoded bytes inline for the
// server to store. Bytes and MediaType describe the decoded image so the row
// carries its size and type without fetching the blob.
type AttachmentDelta struct {
	MessageOrdinal int
	SHA256         string
	Body           string
	Bytes          int64
	MediaType      string
	Filename       string
}

// ProjFallback is one model-fallback observation: a Claude Fable turn the safety
// classifier declined and re-served on a lower model. One logical fallback
// arrives across several transcript lines that share DedupKey (Claude splits one
// API message into several assistant entries, plus a separate system entry for
// the refusal detail), so the fold merges observations on DedupKey: the
// assistant side brings MessageOrdinal and the declined token counts, the system
// side brings Trigger/RefusalCategory/RefusalExplanation, and each field fills
// from whichever line carried it. A field the source did not observe is left at
// its unset default (MessageOrdinal nil, token counts nil, strings empty) so the
// merge can tell "unset" from a real value and never overwrites a filled field
// with a blank.
type ProjFallback struct {
	MessageOrdinal     *int
	FromModel          string
	ToModel            string
	Trigger            string
	RefusalCategory    string
	RefusalExplanation string
	DeclinedInput      *int
	DeclinedOutput     *int
	DeclinedCacheWrite *int
	DeclinedCacheRead  *int
	OccurredAt         time.Time
	DedupKey           string
}

// ProjectionDelta is everything one whole-session parse produces: the rows to
// write and the session's timestamp span. It carries the reducer's raw view;
// the rebuild's in-memory fold (dedup, result patching, fallback merge, prompt
// facts, rollups) runs over it with the complete session in hand, so no row or
// counter here needs to be correct under partial information.
type ProjectionDelta struct {
	Messages    []MessageDelta
	ToolCalls   []ProjToolCall
	ToolResults []ToolResultDelta
	Usage       []ProjUsage
	Attachments []AttachmentDelta
	Fallbacks   []ProjFallback

	Started time.Time
	Ended   time.Time
}

// roleUser is the message role that counts toward user_message_count. The reducer
// emits it as the normalized parser.RoleUser string; the store compares the stored
// string so it does not depend on the parser package.
const roleUser = "user"

// SessionReducer folds a session's raw bytes into one whole-session delta.
// RebuildSession constructs one per rebuild, feeds it every stored region in
// offset order, and calls Finish exactly once for the completed delta. Feed is
// pure CPU: it runs inside the rebuild transaction, so it must not perform I/O.
type SessionReducer interface {
	Feed(region []byte, baseOffset int64) error
	Finish() ProjectionDelta
}

// RebuildSession rebuilds a session's entire projection from its stored raw
// bytes in one transaction: it streams the raw regions through the reducer,
// folds the resulting whole-session delta in memory (usage dedup, tool-result
// patching, fallback merging, prompt facts, rollups), deletes the old derived
// rows, bulk-inserts the new ones, stamps the parse cursor and epoch, and
// re-grades the session's signals. It is the ONLY write path for derived data:
// ingest appends raw bytes and wakes the parse worker, and the worker calls
// this.
//
// Because the delete and the rewrite commit together, a concurrent reader never
// sees the session empty or half built: it sees the prior projection until this
// transaction commits, then the new one. Any failure rolls the whole rebuild
// back, so a parser error on malformed bytes or an operational store/CAS error
// leaves the prior projection intact rather than a cleared session.
//
// The raw bytes are never modified. Raw regions are read in bounded batches
// (rebuildRegionBytes), so the raw side of the parse never holds the whole
// transcript in one buffer; the folded delta is whole-session by design.
//
// Failures split in two. A reducer error (r.Feed rejecting the bytes) is
// deterministic: re-running would fail identically, so the error is recorded on
// session_raw.parse_error and the bookkeeping is stamped as consumed at the
// byte length this parse read, all committed with no projection writes (the
// feed runs before any). The prior projection survives, and the session leaves
// the due set until its bytes or the epoch move, rather than hot-looping on the
// same bad bytes; a chunk that landed mid-parse keeps it due (the stamp records
// the length that was read, not the newer one). The reducer's error is still
// returned so the worker can log and count it. An operational error (a store or
// CAS failure, a cancellation) rolls everything back and stamps nothing, so the
// next drain retries.
func (s *Store) RebuildSession(ctx context.Context, sessionID int64, epoch int, r SessionReducer) error {
	var feedErr error
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		// Lock the parent session row before session_raw. DeleteSession locks
		// sessions first and cascades into session_raw, so taking the two rows in
		// that same order here keeps a concurrent delete and rebuild from deadlocking.
		if err := lockSession(ctx, tx, sessionID); err != nil {
			return err
		}
		byteLen, err := lockSessionRaw(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		var parsedLen int64
		for parsedLen < byteLen {
			region, regionEnd, err := readRawRegion(ctx, tx, sessionID, parsedLen, rebuildRegionBytes)
			if err != nil {
				return err
			}
			if err := r.Feed(region, parsedLen); err != nil {
				feedErr = fmt.Errorf("parse session %d region [%d,%d): %w", sessionID, parsedLen, regionEnd, err)
				if _, serr := tx.Exec(ctx,
					`UPDATE session_raw
					    SET parsed_byte_len = $2, parser_epoch = $3, parse_error = $4
					  WHERE session_id = $1`,
					sessionID, byteLen, epoch, sanitizeText(feedErr.Error())); serr != nil {
					return fmt.Errorf("record parse error for session %d: %w", sessionID, serr)
				}
				return nil // commit the stamp; the parse error itself is returned below
			}
			parsedLen = regionEnd
		}
		return rebuildTx(ctx, tx, sessionID, epoch, byteLen, r.Finish())
	})
	if err != nil {
		return err
	}
	return feedErr
}

// rebuildTx replaces a session's derived rows with the folded form of one
// whole-session delta, inside the caller's transaction (which must hold the
// session and session_raw locks).
func rebuildTx(ctx context.Context, tx pgx.Tx, sessionID int64, epoch int, byteLen int64, d ProjectionDelta) error {
	// Pin every blob the OLD projection references (lifted tool inputs and
	// results, and image attachments) before deleting the rows that reference
	// them, so a concurrent sweep cannot reclaim a blob the rewrite is about to
	// re-reference. The pin (FOR KEY SHARE via the FK) conflicts with the sweep's
	// FOR UPDATE.
	if err := pinSessionBlobsTx(ctx, tx, sessionID); err != nil {
		return err
	}
	for _, q := range []string{
		"DELETE FROM messages WHERE session_id = $1",
		"DELETE FROM tool_calls WHERE session_id = $1",
		"DELETE FROM usage_events WHERE session_id = $1",
		"DELETE FROM message_turn_usage WHERE session_id = $1",
		"DELETE FROM attachments WHERE session_id = $1",
		"DELETE FROM model_fallbacks WHERE session_id = $1",
	} {
		if _, err := tx.Exec(ctx, q, sessionID); err != nil {
			return fmt.Errorf("clear projection for rebuild of session %d (%s): %w", sessionID, q, err)
		}
	}

	if err := writeMessages(ctx, tx, sessionID, d.Messages); err != nil {
		return err
	}
	if err := writeToolCalls(ctx, tx, sessionID, d.ToolCalls, d.ToolResults); err != nil {
		return err
	}
	roll, err := writeUsage(ctx, tx, sessionID, d.Usage)
	if err != nil {
		return err
	}
	if err := writeAttachments(ctx, tx, sessionID, d.Attachments); err != nil {
		return err
	}
	fallbackCount, err := writeFallbacks(ctx, tx, sessionID, d.Fallbacks)
	if err != nil {
		return err
	}

	// The rollups are set absolutely from the folded rows, never incremented, so
	// they equal the ledger by construction (sessions.total_* == sum over
	// usage_events, message_count == count of messages rows). signals_stale is
	// set here and re-settled by refreshSignalsTx below: it stays true for a
	// still-live session (whose outcome is not yet stable) and clears for a
	// settled or terminal one.
	userMessages := 0
	for _, m := range d.Messages {
		if m.Role == roleUser {
			userMessages++
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE sessions SET
		   message_count = $2,
		   user_message_count = $3,
		   model_fallback_count = $4,
		   total_input_tokens = $5,
		   total_output_tokens = $6,
		   total_cache_write_tokens = $7,
		   total_cache_read_tokens = $8,
		   total_cost_usd = $9,
		   cost_incomplete = $10,
		   total_cache_savings_usd = $11,
		   cache_savings_incomplete = $12,
		   started_at = $13,
		   ended_at = $14,
		   updated_at = now(),
		   signals_stale = true
		 WHERE id = $1`,
		sessionID, len(d.Messages), userMessages, fallbackCount,
		roll.input, roll.output, roll.cacheWrite, roll.cacheRead,
		roll.costUSD, roll.costIncomplete,
		roll.cacheSavingsUSD, roll.cacheSavingsIncomplete,
		nullTime(d.Started), nullTime(d.Ended)); err != nil {
		return fmt.Errorf("update aggregates for session %d: %w", sessionID, err)
	}

	// Stamp the cursor and epoch LAST, in the same transaction as the rows they
	// describe: a session is due for rebuild exactly while parsed_byte_len <>
	// byte_len or parser_epoch <> the running epoch, so a crash before commit
	// leaves it due and the next worker pass retries from the raw bytes.
	if _, err := tx.Exec(ctx,
		`UPDATE session_raw
		    SET parsed_byte_len = $2, parser_epoch = $3, parse_error = ''
		  WHERE session_id = $1`,
		sessionID, byteLen, epoch); err != nil {
		return fmt.Errorf("stamp parse cursor for session %d: %w", sessionID, err)
	}

	// The projection is fully rebuilt; recompute the session's signals from it in
	// this same transaction when the session is gradeable (settled past the idle
	// window, or declared terminal), so the grade commits atomically with the
	// projection it summarizes. A still-live session instead has its now-stale
	// signals row dropped and stays flagged signals_stale: its outcome is
	// time-dependent (abandoned versus unknown turns on the idle gap), so storing
	// a verdict now would bake in a value that drifts, and the settle tick grades
	// it once it crosses the threshold.
	var gradeable bool
	if err := tx.QueryRow(ctx,
		`SELECT s.terminal OR (s.ended_at IS NOT NULL AND s.ended_at < now() - make_interval(mins => $2))
		   FROM sessions s WHERE s.id = $1`,
		sessionID, abandonedIdleMinutes).Scan(&gradeable); err != nil {
		return fmt.Errorf("check gradeability for session %d: %w", sessionID, err)
	}
	if !gradeable {
		if _, err := tx.Exec(ctx,
			`DELETE FROM session_signals WHERE session_id = $1`, sessionID); err != nil {
			return fmt.Errorf("drop stale signals for session %d: %w", sessionID, err)
		}
		return nil
	}
	return refreshSignalsTx(ctx, tx, sessionID)
}

// writeMessages derives each user turn's prompt facts and duplicate verdict over
// the complete ordered transcript, then bulk-inserts the rows. The duplicate
// verdict needs only an in-memory digest set: the fold sees every earlier turn,
// so no per-insert index probe or window scan is involved. Facts columns are
// NULL on non-user rows, so the hygiene aggregate's role='user' reads see
// exactly the classified set.
func writeMessages(ctx context.Context, tx pgx.Tx, sessionID int64, msgs []MessageDelta) error {
	if len(msgs) == 0 {
		return nil
	}
	rows := make([][]any, 0, len(msgs))
	seenDigests := map[int64]bool{}
	for _, m := range msgs {
		content := sanitizeText(m.Content)
		var pShort, pNoCode, pGreeting, pDigest, pDuplicate any
		if m.Role == roleUser {
			facts := quality.ClassifyPrompt(content)
			pShort, pNoCode, pGreeting, pDigest = facts.Short, facts.NoCodeContext, facts.BareGreeting, facts.Digest
			// Only duplicate-eligible turns (non-empty, not short) carry a verdict; an
			// ineligible row stays NULL (no badge), matching gatherPromptHygiene's
			// exclusion of short and empty prompts from the duplicate count.
			if content != "" && !facts.Short {
				pDuplicate = seenDigests[facts.Digest]
				seenDigests[facts.Digest] = true
			}
		}
		rows = append(rows, []any{
			sessionID, m.Ordinal, sanitizeText(m.Role), content,
			sanitizeText(m.ThinkingText), m.ThinkingBytes, sanitizeText(m.Model),
			nullTime(m.Timestamp), m.HasThinking, m.HasToolUse,
			pShort, pNoCode, pGreeting, pDigest, pDuplicate,
		})
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"messages"},
		[]string{"session_id", "ordinal", "role", "content", "thinking_text", "thinking_bytes", "model",
			"timestamp", "has_thinking", "has_tool_use",
			"prompt_short", "prompt_no_code", "prompt_bare_greeting", "prompt_digest", "duplicate_prompt"},
		pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("copy messages for session %d: %w", sessionID, err)
	}
	return nil
}

// writeToolCalls resolves each call's input body into the CAS (writing inline
// bodies, pinning client-lifted references), patches results onto their calls by
// call id, and bulk-inserts the completed rows.
//
// Result patching runs over the complete session, so a call and its result meet
// here no matter how many regions apart they were logged. A result patches every
// still-unresolved call sharing its id: a resumed or compacted Claude transcript
// replays prior turns verbatim, so the same call_uid legitimately rides several
// rows, and each visible copy should carry the same result rather than one
// looking pending. A second result for an already-resolved id is dropped, the
// same first-result-wins the old pending-only back-patch had.
func writeToolCalls(ctx context.Context, tx pgx.Tx, sessionID int64, calls []ProjToolCall, results []ToolResultDelta) error {
	if len(calls) == 0 {
		return nil
	}
	// Load the session's cwd once for the whole rebuild: it anchors each call's
	// worktree-invariant relative path. It is empty when the session announced no
	// cwd, which sessionRelPath treats as "no anchor" (relative path NULL for
	// absolute paths).
	var sessionCwd string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(cwd, '') FROM sessions WHERE id = $1`, sessionID).Scan(&sessionCwd); err != nil {
		return fmt.Errorf("load cwd for session %d: %w", sessionID, err)
	}

	// resolved carries each call's result columns once a result patches it.
	type resolvedResult struct {
		sha       any
		bytes     int64
		mediaType string
		status    string
		set       bool
	}
	resolved := make([]resolvedResult, len(calls))
	byUID := map[string][]int{}
	for i, t := range calls {
		// Key on the sanitized id: the row stores the sanitized form, so the match
		// must compare like with like when a transcript id carried invalid bytes.
		if uid := sanitizeText(t.CallUID); uid != "" {
			byUID[uid] = append(byUID[uid], i)
		}
	}
	for _, tr := range results {
		if tr.CallUID == "" {
			continue
		}
		idxs := byUID[sanitizeText(tr.CallUID)]
		// Write or pin the result body once per result, but only when at least one
		// call is still pending: a result whose id matches nothing (or whose copies
		// are all resolved) stores no blob.
		var sha any
		stored := false
		for _, i := range idxs {
			if resolved[i].set {
				continue
			}
			if !stored {
				switch {
				case tr.BodySHA256 != "":
					if err := pinBlobRefTx(ctx, tx, tr.BodySHA256); err != nil {
						return fmt.Errorf("reference tool result blob %s for session %d call %q: %w", tr.BodySHA256, sessionID, tr.CallUID, err)
					}
					sha = tr.BodySHA256
				case len(tr.Body) > 0:
					s, err := writeBlobTx(ctx, tx, tr.Body, tr.MediaType)
					if err != nil {
						return fmt.Errorf("write tool result blob for session %d call %q: %w", sessionID, tr.CallUID, err)
					}
					sha = s
				}
				stored = true
			}
			media := sanitizeText(tr.MediaType)
			if media == "" {
				media = "text/plain"
			}
			resolved[i] = resolvedResult{sha: sha, bytes: tr.Bytes, mediaType: media, status: sanitizeText(tr.Status), set: true}
		}
	}

	rows := make([][]any, 0, len(calls))
	for i, t := range calls {
		var inputSHA, inputMedia any
		switch {
		case t.InputSHA256 != "":
			// The client lifted the input to the CAS and left a sentinel; the blob is
			// already present (and pinned against the sweep), so record the reference
			// without re-storing the body. Re-lock it FOR KEY SHARE so a sweep racing
			// this insert cannot delete the blob between here and the FK check.
			if err := pinBlobRefTx(ctx, tx, t.InputSHA256); err != nil {
				return fmt.Errorf("reference tool input blob %s for session %d call %d/%d: %w", t.InputSHA256, sessionID, t.MessageOrdinal, t.CallIndex, err)
			}
			inputSHA, inputMedia = t.InputSHA256, sanitizeText(t.InputMediaType)
		case len(t.InputBody) > 0:
			sha, err := writeBlobTx(ctx, tx, t.InputBody, t.InputMediaType)
			if err != nil {
				return fmt.Errorf("write tool input blob for session %d call %d/%d: %w", sessionID, t.MessageOrdinal, t.CallIndex, err)
			}
			inputSHA, inputMedia = sha, sanitizeText(t.InputMediaType)
		}
		// Store the session-relative form of the path beside the absolute one.
		// file_path is absolute, so the same repo file edited from two worktrees of
		// one repo fragments into separate churn rows; file_rel_path is the
		// worktree-invariant key that (paired with the project, which already
		// collapses worktrees on the canonical remote) collapses them back together.
		// It is NULL when no stable relative form exists (a path outside the
		// workspace, or an absolute path with no announced cwd), which churn
		// coalesces back onto file_path so an unanchored edit still counts under its
		// absolute name rather than vanishing.
		var relPath any
		if rel, ok := sessionRelPath(sessionCwd, sanitizeText(t.FilePath)); ok {
			relPath = rel
		}
		var resSHA, resBytes, resMedia, resStatus any
		if resolved[i].set {
			resSHA, resBytes, resMedia, resStatus = resolved[i].sha, resolved[i].bytes, resolved[i].mediaType, resolved[i].status
		}
		rows = append(rows, []any{
			sessionID, t.MessageOrdinal, t.CallIndex, sanitizeText(t.ToolName), sanitizeText(t.Category),
			nullString(sanitizeText(t.FilePath)), relPath,
			inputSHA, t.InputBytes, inputMedia, nullString(sanitizeText(t.CallUID)),
			nullString(sanitizeText(t.Detail)),
			resSHA, resBytes, resMedia, resStatus,
		})
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"tool_calls"},
		[]string{"session_id", "message_ordinal", "call_index", "tool_name", "category",
			"file_path", "file_rel_path",
			"input_sha256", "input_bytes", "input_media_type", "call_uid", "detail",
			"result_sha256", "result_bytes", "result_media_type", "result_status"},
		pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("copy tool calls for session %d: %w", sessionID, err)
	}
	return nil
}

// usageRollup is the session-total fold over the surviving usage rows: the
// figures rebuildTx writes onto the sessions row.
type usageRollup struct {
	input, output, cacheWrite, cacheRead int64
	costUSD                              float64
	costIncomplete                       bool
	cacheSavingsUSD                      float64
	cacheSavingsIncomplete               bool
}

// writeUsage dedups the reducer's usage events in memory, bulk-inserts the
// survivors and their per-turn rollup, and returns the session totals.
//
// The dedup mirrors the two unique indexes on usage_events, which remain as
// integrity backstops: rows sharing a non-empty dedup_key collapse to their
// first occurrence (Claude logs one API response's usage on every content-block
// line of the message), and rows sharing (source_offset, source_index) collapse
// likewise (one transcript line is one event). First-occurrence-wins matches the
// old ON CONFLICT DO NOTHING insert order.
func writeUsage(ctx context.Context, tx pgx.Tx, sessionID int64, usage []ProjUsage) (usageRollup, error) {
	var roll usageRollup
	if len(usage) == 0 {
		return roll, nil
	}
	type sourceKey struct {
		offset int64
		index  int
	}
	seenKey := map[string]bool{}
	seenSource := map[sourceKey]bool{}
	rows := make([][]any, 0, len(usage))

	// Per-turn rollup, keyed by ordinal, accumulated over the same surviving set.
	type turnAgg struct {
		input, output, cacheWrite, cacheRead, reasoning int64
		costSum                                         float64
		costCount                                       int64
		costIncomplete                                  bool
	}
	turns := map[int]*turnAgg{}
	var turnOrder []int

	for _, u := range usage {
		key := sanitizeText(u.DedupKey)
		if key != "" && seenKey[key] {
			continue
		}
		src := sourceKey{u.SourceOffset, u.SourceIndex}
		if seenSource[src] {
			continue
		}
		if key != "" {
			seenKey[key] = true
		}
		seenSource[src] = true

		var ord, cost any
		if u.MessageOrdinal != nil {
			ord = *u.MessageOrdinal
		}
		if u.CostUSD != nil {
			cost = *u.CostUSD
		}
		rows = append(rows, []any{
			sessionID, ord, sanitizeText(u.Model), u.Input, u.Output, u.CacheWrite, u.CacheRead,
			u.Reasoning, cost, nullTime(u.OccurredAt), key, u.SourceOffset, u.SourceIndex,
		})

		hasTokens := u.Input+u.Output+u.CacheWrite+u.CacheRead+u.Reasoning > 0
		// Fold this surviving row into its turn's rollup, so the transcript reads one
		// row per turn instead of re-grouping usage_events on every render. A
		// NULL-ordinal row belongs to the session totals, not to any one message.
		if u.MessageOrdinal != nil {
			t := turns[*u.MessageOrdinal]
			if t == nil {
				t = &turnAgg{}
				turns[*u.MessageOrdinal] = t
				turnOrder = append(turnOrder, *u.MessageOrdinal)
			}
			t.input += int64(u.Input)
			t.output += int64(u.Output)
			t.cacheWrite += int64(u.CacheWrite)
			t.cacheRead += int64(u.CacheRead)
			t.reasoning += int64(u.Reasoning)
			if u.CostUSD != nil {
				t.costSum += *u.CostUSD
				t.costCount++
			} else if hasTokens {
				t.costIncomplete = true
			}
		}

		roll.input += int64(u.Input)
		roll.output += int64(u.Output)
		roll.cacheWrite += int64(u.CacheWrite)
		roll.cacheRead += int64(u.CacheRead)
		switch {
		case u.CostUSD != nil:
			roll.costUSD += *u.CostUSD
		case hasTokens:
			// Tokens spent on a model the pricing table does not know: the session
			// total is a partial sum and the flag says so.
			roll.costIncomplete = true
		}
		// Fold the prompt-cache saving over the same surviving rows. Pricing is
		// linear in tokens, so summing each row's saving equals summing the model's
		// grouped totals (what SessionCacheStats does over the whole session), which
		// is what lets the rollup and that per-model recompute reconcile exactly.
		if saving, ok := pricing.CacheSavings(u.Model, u.OccurredAt, int64(u.CacheRead), int64(u.CacheWrite)); ok {
			roll.cacheSavingsUSD += saving
		} else if u.CacheRead > 0 || u.CacheWrite > 0 {
			roll.cacheSavingsIncomplete = true
		}
	}

	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"usage_events"},
		[]string{"session_id", "message_ordinal", "model", "input_tokens", "output_tokens",
			"cache_write_tokens", "cache_read_tokens", "reasoning_tokens", "cost_usd",
			"occurred_at", "dedup_key", "source_offset", "source_index"},
		pgx.CopyFromRows(rows)); err != nil {
		return usageRollup{}, fmt.Errorf("copy usage events for session %d: %w", sessionID, err)
	}

	if len(turnOrder) > 0 {
		turnRows := make([][]any, 0, len(turnOrder))
		for _, ord := range turnOrder {
			t := turns[ord]
			var costSum any
			if t.costCount > 0 {
				costSum = t.costSum
			} else {
				costSum = 0.0
			}
			turnRows = append(turnRows, []any{
				sessionID, ord, t.input, t.output, t.cacheWrite, t.cacheRead, t.reasoning,
				costSum, t.costCount, t.costIncomplete,
			})
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"message_turn_usage"},
			[]string{"session_id", "message_ordinal", "input_tokens", "output_tokens",
				"cache_write_tokens", "cache_read_tokens", "reasoning_tokens",
				"cost_sum", "cost_count", "cost_incomplete"},
			pgx.CopyFromRows(turnRows)); err != nil {
			return usageRollup{}, fmt.Errorf("copy turn usage for session %d: %w", sessionID, err)
		}
	}
	return roll, nil
}

// writeAttachments stores each attachment's body (or pins its client-lifted
// reference) and bulk-inserts the rows. The reducer dedups attachments by
// content key across the whole session, so the rows are unique by construction.
func writeAttachments(ctx context.Context, tx pgx.Tx, sessionID int64, atts []AttachmentDelta) error {
	if len(atts) == 0 {
		return nil
	}
	rows := make([][]any, 0, len(atts))
	for _, a := range atts {
		var sha string
		switch {
		case a.SHA256 != "":
			if err := pinBlobRefTx(ctx, tx, a.SHA256); err != nil {
				return fmt.Errorf("reference attachment blob %s for session %d ordinal %d: %w", a.SHA256, sessionID, a.MessageOrdinal, err)
			}
			sha = a.SHA256
		case len(a.Body) > 0:
			s, err := writeBlobTx(ctx, tx, a.Body, a.MediaType)
			if err != nil {
				return fmt.Errorf("write attachment blob for session %d ordinal %d: %w", sessionID, a.MessageOrdinal, err)
			}
			sha = s
		default:
			continue // an attachment with no body is nothing to store
		}
		media := sanitizeText(a.MediaType)
		if media == "" {
			media = "application/octet-stream"
		}
		rows = append(rows, []any{
			sessionID, a.MessageOrdinal, sha, nullString(sanitizeText(a.Filename)), media, a.Bytes,
		})
	}
	if len(rows) == 0 {
		return nil
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"attachments"},
		[]string{"session_id", "message_ordinal", "sha256", "filename", "media_type", "byte_len"},
		pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("copy attachments for session %d: %w", sessionID, err)
	}
	return nil
}

// writeFallbacks merges the reducer's fallback observations by dedup key and
// bulk-inserts one row per logical fallback, returning how many there were (the
// sessions.model_fallback_count rollup).
//
// The merge fills each field from whichever line carried it. message_ordinal is
// first-wins: it binds to the assistant line that owns the turn, staying in
// lockstep with the usage fold (which also keeps the first assistant line's
// event), and the system entry's NULL never moves it. occurred_at tracks
// whichever line owns that ordinal: once an assistant line has set the ordinal
// the timestamp is frozen to that line's, so a system entry's later timestamp
// never drifts the notice off the turn the usage describes. The descriptive
// fields are fill-toward-complete: a later non-empty value overrides an earlier
// empty one (trigger/category/explanation live only on the system entry, the
// declined token counts only on the specific assistant line), and a blank never
// clears a filled value.
func writeFallbacks(ctx context.Context, tx pgx.Tx, sessionID int64, fbs []ProjFallback) (int, error) {
	if len(fbs) == 0 {
		return 0, nil
	}
	merged := map[string]*ProjFallback{}
	var order []string
	for _, fb := range fbs {
		key := sanitizeText(fb.DedupKey)
		m, ok := merged[key]
		if !ok {
			cp := fb
			cp.DedupKey = key
			merged[key] = &cp
			order = append(order, key)
			continue
		}
		hadOrdinal := m.MessageOrdinal != nil
		if m.MessageOrdinal == nil {
			m.MessageOrdinal = fb.MessageOrdinal
		}
		if fb.FromModel != "" {
			m.FromModel = fb.FromModel
		}
		if fb.ToModel != "" {
			m.ToModel = fb.ToModel
		}
		if fb.Trigger != "" {
			m.Trigger = fb.Trigger
		}
		if fb.RefusalCategory != "" {
			m.RefusalCategory = fb.RefusalCategory
		}
		if fb.RefusalExplanation != "" {
			m.RefusalExplanation = fb.RefusalExplanation
		}
		if fb.DeclinedInput != nil {
			m.DeclinedInput = fb.DeclinedInput
		}
		if fb.DeclinedOutput != nil {
			m.DeclinedOutput = fb.DeclinedOutput
		}
		if fb.DeclinedCacheWrite != nil {
			m.DeclinedCacheWrite = fb.DeclinedCacheWrite
		}
		if fb.DeclinedCacheRead != nil {
			m.DeclinedCacheRead = fb.DeclinedCacheRead
		}
		switch {
		case hadOrdinal:
			// The turn-owning line already stamped the timestamp; keep it.
		case fb.MessageOrdinal != nil:
			m.OccurredAt = fb.OccurredAt
		case m.OccurredAt.IsZero():
			m.OccurredAt = fb.OccurredAt
		}
	}
	rows := make([][]any, 0, len(order))
	for _, key := range order {
		m := merged[key]
		var ord any
		if m.MessageOrdinal != nil {
			ord = *m.MessageOrdinal
		}
		rows = append(rows, []any{
			sessionID, ord, sanitizeText(m.FromModel), sanitizeText(m.ToModel), sanitizeText(m.Trigger),
			nullString(sanitizeText(m.RefusalCategory)), nullString(sanitizeText(m.RefusalExplanation)),
			nullInt(m.DeclinedInput), nullInt(m.DeclinedOutput),
			nullInt(m.DeclinedCacheWrite), nullInt(m.DeclinedCacheRead),
			nullTime(m.OccurredAt), key,
		})
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"model_fallbacks"},
		[]string{"session_id", "message_ordinal", "from_model", "to_model", "trigger",
			"refusal_category", "refusal_explanation",
			"declined_input_tokens", "declined_output_tokens",
			"declined_cache_write_tokens", "declined_cache_read_tokens",
			"occurred_at", "dedup_key"},
		pgx.CopyFromRows(rows)); err != nil {
		return 0, fmt.Errorf("copy model fallbacks for session %d: %w", sessionID, err)
	}
	return len(rows), nil
}

// readRawRegion concatenates the raw chunks for one feed batch: the chunks whose
// start falls in [from, from+cap). The bound is in SQL, so a rebuild fetches
// only each batch's chunks rather than rescanning the whole tail every time.
// Chunks are contiguous and line aligned, so the returned region always ends on
// a JSONL line boundary, and the chunk at `from` always qualifies (so a batch is
// never empty when bytes remain). It returns the bytes and the offset just past
// them.
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

// lockSessionRaw locks the session_raw row (the caller must already hold the parent
// session row, per the (session, session_raw) order DeleteSession takes) and returns
// the stored byte length.
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

// lockSession takes the row lock on the parent session before the rebuild path
// locks session_raw and updates the session aggregates. DeleteSession locks
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

func nullInt(n *int) any {
	if n == nil {
		return nil
	}
	return *n
}

// sanitizeText makes a session-derived string safe for a Postgres text column.
// Postgres rejects a NUL byte (0x00) outright and any byte sequence that is not
// valid UTF-8, and a single offending byte fails the whole INSERT: that is how a
// rebuild of one Claude session (a message body carrying a raw NUL) rolled back
// and kept its stale projection. Replacing the bad bytes with U+FFFD keeps the
// row writable and marks where the transcript was malformed, rather than dropping
// the message or stranding the session. NUL is itself valid UTF-8, so ToValidUTF8
// leaves it in place and it is replaced separately; both calls return the input
// unchanged when there is nothing to fix, so the clean path does not allocate.
func sanitizeText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "�")
}
