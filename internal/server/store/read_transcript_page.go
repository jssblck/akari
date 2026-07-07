package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
)

// The web transcript renders a bounded window instead of the whole session (an unbounded
// server render is what froze the tab on long sessions). These constants bound every
// windowed read:
//
//   - TranscriptTailTurns: how many user turns the initial page and each "Show earlier"
//     fetch cover. A turn is one user message plus the assistant run that follows, so the
//     window boundary always lands on a prompt, never mid-answer.
//   - transcriptPageMessageCap: the hard message bound behind the turn count, so a
//     pathological session (one prompt, thousands of assistant rows) still cannot blow
//     up a single fetch.
//   - transcriptSeedLookback: how many rows immediately before a window ride along
//     unrendered, to prime the render walker's carried state (reply latency, shed
//     detection) at the window boundary. A prompt is almost always within a few rows of
//     its reply, so a short fixed lookback recovers the boundary instruments without
//     scaling the read; a turn whose anchor sits deeper than the lookback shows no
//     latency stamp, which the plan accepts.
const (
	TranscriptTailTurns      = 50
	transcriptPageMessageCap = 600
	transcriptSeedLookback   = 8
)

// TranscriptPage is one contiguous, full-fold window of a session's transcript: the rows
// to render, the unrendered seed rows that precede them, and what lies beyond the window
// on each side. Every row carries the same per-turn usage and duplicate-prompt fold the
// whole-session read carries, so a windowed transcript renders identically to the full one.
type TranscriptPage struct {
	// Msgs is the window itself, in ordinal order.
	Msgs []Message
	// Seed is up to transcriptSeedLookback rows immediately before Msgs, in ordinal
	// order, for walker priming only; the caller must not render them.
	Seed []Message
	// Tools, Attachments, and Fallbacks are the tool calls, attachments, and model
	// fallback notices hanging on Msgs, read in the same transaction as the rows
	// themselves. A rebuild committing between separate reads could otherwise pair one
	// projection's messages with another's chips, images, or notices at the same
	// ordinals, and an appended fragment would leave that mix in the DOM; carrying them
	// on the page pins all four to one snapshot.
	Tools       []ToolCallView
	Attachments []AttachmentView
	Fallbacks   []ModelFallback
	// HasEarlier reports whether any rows precede the window, so the renderer knows to
	// draw the "Show earlier" bar. EarlierCount is how many (for the bar's label).
	HasEarlier   bool
	EarlierCount int
	// More reports that rows beyond the window remain AFTER it. Only TranscriptAfter
	// sets it: a live append that hits the message cap means the client is too far
	// behind for an append to reconcile, and the handler should re-render the window
	// whole instead of leaving a gap in the DOM.
	More bool
}

// snapshotTx runs fn inside a repeatable-read, read-only transaction, so the several
// reads behind one page (a transcript window with its tools and attachments, or the
// audit header's cost-bearing rows) describe one MVCC snapshot even if the parse
// worker commits a rebuild mid-request.
func (s *Store) snapshotTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.Pool,
		pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, fn)
}

// TranscriptTail reads the trailing window of a session's transcript: up to
// TranscriptTailTurns user turns (bounded by transcriptPageMessageCap messages) ending at
// the transcript's end, or strictly before `before` when the reader is paging earlier
// ("Show earlier" passes its first rendered ordinal). The window starts on a turn
// boundary (a user message) whenever one exists within the cap; a session with fewer
// turns than the window simply starts at its beginning.
func (s *Store) TranscriptTail(ctx context.Context, sessionID int64, before *int) (TranscriptPage, error) {
	var page TranscriptPage
	err := s.snapshotTx(ctx, func(tx pgx.Tx) error {
		var err error
		page, err = s.transcriptTail(ctx, tx, sessionID, before)
		return err
	})
	if err != nil {
		return TranscriptPage{}, err
	}
	return page, nil
}

// transcriptTail is TranscriptTail inside a caller-owned transaction, so the session
// snapshot reads can pin the window to the same MVCC snapshot as the audit and shape
// rows beside it.
func (s *Store) transcriptTail(ctx context.Context, tx pgx.Tx, sessionID int64, before *int) (TranscriptPage, error) {
	var page TranscriptPage
	// The window's start: the TranscriptTailTurns-th user message counting back from
	// the window's end. Walks the (session_id, ordinal) primary key backward; no row
	// means the session has fewer turns than the window, so start at the beginning.
	start := 0
	err := tx.QueryRow(ctx,
		`SELECT m.ordinal FROM messages m
		  WHERE m.session_id = $1 AND m.role = 'user' AND ($2::int IS NULL OR m.ordinal < $2)
		  ORDER BY m.ordinal DESC OFFSET $3 LIMIT 1`,
		sessionID, before, TranscriptTailTurns-1).Scan(&start)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return page, fmt.Errorf("find tail window start for session %d: %w", sessionID, err)
	}
	// Read the window newest-first so the cap keeps the rows nearest the window's end
	// (the reader pages backward from the live edge), then restore ordinal order.
	msgs, err := s.scanMessages(ctx, tx, sessionID,
		messagesFullSelect+` AND ($2::int IS NULL OR m.ordinal < $2) AND m.ordinal >= $3
		 ORDER BY m.ordinal DESC LIMIT $4`,
		sessionID, before, start, transcriptPageMessageCap)
	if err != nil {
		return page, err
	}
	slices.Reverse(msgs)
	page.Msgs = msgs
	if len(msgs) == 0 {
		// An empty window (an empty transcript, or a cursor at the very start) has
		// nothing before it either: the window predicate already reached ordinal >= 0.
		return page, nil
	}
	if err := s.fillWindowExtras(ctx, tx, sessionID, &page); err != nil {
		return page, err
	}
	return page, s.fillWindowLead(ctx, tx, sessionID, msgs[0].Ordinal, &page)
}

// TranscriptAfter reads the rows strictly after `after`, for the live SSE append: the
// client names the last ordinal it rendered and receives just the turns that follow,
// with the seed rows that let the walker stamp the boundary. When more rows exist than
// the cap, More is set and the caller should fall back to a whole-window re-render
// rather than append a fragment with a gap after it.
func (s *Store) TranscriptAfter(ctx context.Context, sessionID int64, after int) (TranscriptPage, error) {
	var page TranscriptPage
	err := s.snapshotTx(ctx, func(tx pgx.Tx) error {
		var err error
		page, err = s.transcriptAfter(ctx, tx, sessionID, after)
		return err
	})
	if err != nil {
		return TranscriptPage{}, err
	}
	return page, nil
}

// transcriptAfter is TranscriptAfter inside a caller-owned transaction (see
// transcriptTail).
func (s *Store) transcriptAfter(ctx context.Context, tx pgx.Tx, sessionID int64, after int) (TranscriptPage, error) {
	var page TranscriptPage
	msgs, err := s.scanMessages(ctx, tx, sessionID,
		messagesFullSelect+` AND m.ordinal > $2 ORDER BY m.ordinal LIMIT $3`,
		sessionID, after, transcriptPageMessageCap+1)
	if err != nil {
		return page, err
	}
	if len(msgs) > transcriptPageMessageCap {
		page.More = true
		msgs = msgs[:transcriptPageMessageCap]
	}
	page.Msgs = msgs
	if err := s.fillWindowExtras(ctx, tx, sessionID, &page); err != nil {
		return page, err
	}
	// The seed here includes the row AT `after` (the last one the client already
	// holds): `<=` rather than `<`, since the boundary instruments compare the first
	// appended row against exactly that row. It is read even when nothing follows
	// the cursor: a seed ending exactly at `after` is how the caller tells a quiet
	// tick (nothing new yet) from a cursor the projection no longer has (an epoch
	// rebuild reshaped the transcript), which must force a resync instead.
	seed, err := s.scanMessages(ctx, tx, sessionID,
		messagesFullSelect+` AND m.ordinal <= $2 ORDER BY m.ordinal DESC LIMIT $3`,
		sessionID, after, transcriptSeedLookback)
	if err != nil {
		return page, err
	}
	slices.Reverse(seed)
	page.Seed = seed
	return page, nil
}

// fillWindowExtras loads the tool calls and attachments hanging on the page's window,
// inside the same transaction as the window itself (see TranscriptPage.Tools). An empty
// window carries neither.
func (s *Store) fillWindowExtras(ctx context.Context, tx pgx.Tx, sessionID int64, page *TranscriptPage) error {
	if len(page.Msgs) == 0 {
		return nil
	}
	lo, hi := page.Msgs[0].Ordinal, page.Msgs[len(page.Msgs)-1].Ordinal
	tools, err := s.scanToolCalls(ctx, tx, toolCallsInRangeQuery, sessionID, lo, hi)
	if err != nil {
		return fmt.Errorf("read window tool calls for session %d: %w", sessionID, err)
	}
	page.Tools = tools
	atts, err := s.scanAttachments(ctx, tx, attachmentsInRangeQuery, sessionID, lo, hi)
	if err != nil {
		return fmt.Errorf("read window attachments for session %d: %w", sessionID, err)
	}
	page.Attachments = atts
	// Window-ranged rather than the capped whole-session list the header tile shows:
	// bounded by the window either way, and a notice can never be cut by a cap that
	// tripped on fallbacks outside the window.
	fallbacks, err := s.scanModelFallbacks(ctx, tx, sessionID,
		`SELECT `+modelFallbackColumns+`
		   FROM model_fallbacks WHERE session_id = $1 AND message_ordinal BETWEEN $2 AND $3
		  ORDER BY occurred_at, dedup_key`, sessionID, lo, hi)
	if err != nil {
		return err
	}
	page.Fallbacks = fallbacks
	return nil
}

// fillWindowLead loads what precedes a window that starts at firstOrdinal: the walker
// seed rows and the earlier-row count behind the "Show earlier" bar.
func (s *Store) fillWindowLead(ctx context.Context, tx pgx.Tx, sessionID int64, firstOrdinal int, page *TranscriptPage) error {
	seed, err := s.scanMessages(ctx, tx, sessionID,
		messagesFullSelect+` AND m.ordinal < $2 ORDER BY m.ordinal DESC LIMIT $3`,
		sessionID, firstOrdinal, transcriptSeedLookback)
	if err != nil {
		return err
	}
	slices.Reverse(seed)
	page.Seed = seed
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM messages WHERE session_id = $1 AND ordinal < $2`,
		sessionID, firstOrdinal).Scan(&page.EarlierCount); err != nil {
		return fmt.Errorf("count earlier messages for session %d: %w", sessionID, err)
	}
	page.HasEarlier = page.EarlierCount > 0
	return nil
}

// messagesOutlineQuery is the bounded whole-session read behind OutlineMessages: every
// message row, but with the two content-bearing columns cut down. The outline and the
// flow ribbon need one entry per turn regardless of how the transcript is windowed, yet
// they render at most a 48-rune label per row (web.OutlineTitle scans 256 runes), so
// content is cut in SQL and thinking_text (which can be megabytes across a long session)
// is dropped entirely. The usage columns are the same empty constants the MCP window
// read emits: the outline renders no per-turn usage.
const messagesOutlineQuery = `
	SELECT m.ordinal, m.role, left(m.content, 512), ''::text, m.model, m.has_thinking, m.has_tool_use,
	       coalesce(m.thinking_bytes, 0), m.timestamp,
	       coalesce(m.prompt_short, false), coalesce(m.prompt_no_code, false), coalesce(m.prompt_digest, 0),
	       (m.prompt_digest IS NOT NULL AND m.content_length > 0),
	       coalesce(m.duplicate_prompt, false),
	       false, 0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::bigint, 0::bigint, NULL::double precision, 0::bigint, false
	  FROM messages m
	 WHERE m.session_id = $1
	 ORDER BY m.ordinal`

// OutlineMessages returns every message of a session with bounded columns, for the
// outline rail and the flow ribbon: full coverage (one entry per turn even when the
// transcript window is partial) at a fixed cost per row, independent of message size.
func (s *Store) OutlineMessages(ctx context.Context, sessionID int64) ([]Message, error) {
	return s.scanMessages(ctx, s.Pool, sessionID, messagesOutlineQuery, sessionID)
}
