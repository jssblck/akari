package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// The live session page renders several representations of one transcript side by
// side: the audit header's cost-bearing rows, the windowed transcript with its tools
// and attachments, and the whole-session shape (the outline rail's bounded-column rows
// plus the tool metadata the flow ribbon colors ticks by). The parse worker replaces a
// session's whole projection in one commit, so any two of these read in separate
// transactions can straddle a rebuild and disagree: old window rows beside a new
// outline, ticks colored by another projection's tools, a work-item total that no
// longer matches the row costs beside it. These reads bundle everything one response
// renders into a single repeatable-read snapshot, so a response is always one
// projection or the other, never a mix.

// SessionSnapshot is everything the live session body renders from one MVCC snapshot.
// Outline and Tools are nil when the read skipped the shape (a quiet append tick, see
// SessionAppendByID); the fragment then carries no shape swap, which is right because
// no turns changed.
type SessionSnapshot struct {
	Audit   SessionAudit
	Page    TranscriptPage
	Outline []Message
	Tools   []ToolCallView
	// DupIDs counts tool-call ids appearing on more than one row (a replayed
	// transcript), read with the shape: it summarizes the same tool_calls rows the
	// page renders, so it must not straddle a rebuild against them.
	DupIDs int
}

// SessionSnapshotByID loads the full session view: the audit bundle, the transcript's
// tail window, and the whole-session shape. A missing session returns ErrNotFound.
func (s *Store) SessionSnapshotByID(ctx context.Context, sessionID int64) (SessionSnapshot, error) {
	var snap SessionSnapshot
	err := s.snapshotTx(ctx, func(tx pgx.Tx) error {
		var err error
		if snap.Audit, err = s.sessionAudit(ctx, tx, sessionID); err != nil {
			return err
		}
		if snap.Page, err = s.transcriptTail(ctx, tx, sessionID, nil); err != nil {
			return err
		}
		return s.fillShape(ctx, tx, sessionID, &snap)
	})
	if err != nil {
		return SessionSnapshot{}, err
	}
	return snap, nil
}

// SessionAppendByID loads the live append: the audit bundle (the fragment refreshes
// the instruments out-of-band on every tick) and the rows past `after`, plus the
// whole-session shape only when rows actually landed. A quiet tick (raw bytes ahead of
// the rebuild) changes no turns, so it skips both the shape read and the swap it would
// feed. A missing session returns ErrNotFound.
func (s *Store) SessionAppendByID(ctx context.Context, sessionID int64, after int) (SessionSnapshot, error) {
	var snap SessionSnapshot
	err := s.snapshotTx(ctx, func(tx pgx.Tx) error {
		var err error
		if snap.Audit, err = s.sessionAudit(ctx, tx, sessionID); err != nil {
			return err
		}
		if snap.Page, err = s.transcriptAfter(ctx, tx, sessionID, after); err != nil {
			return err
		}
		if len(snap.Page.Msgs) == 0 {
			return nil
		}
		return s.fillShape(ctx, tx, sessionID, &snap)
	})
	if err != nil {
		return SessionSnapshot{}, err
	}
	return snap, nil
}

// fillShape loads the whole-session shape inside the snapshot's transaction: the
// outline read's bounded-column rows and the full tool metadata. The outline and the
// ribbon derive one picture from both, so the two reads must share the snapshot with
// each other (a tick colored by another projection's tools points at the wrong turn)
// as well as with the window beside them.
func (s *Store) fillShape(ctx context.Context, tx pgx.Tx, sessionID int64, snap *SessionSnapshot) error {
	outline, err := s.scanMessages(ctx, tx, sessionID, messagesOutlineQuery, sessionID)
	if err != nil {
		return err
	}
	snap.Outline = outline
	tools, err := s.scanToolCalls(ctx, tx, toolCallsQuery, sessionID)
	if err != nil {
		return err
	}
	snap.Tools = tools
	snap.DupIDs, err = s.duplicateCallUIDCount(ctx, tx, sessionID)
	return err
}

// SessionEarlierByID loads the "Show earlier" fragment's inputs from one snapshot: the
// session row and the window strictly before `before`. The window's rows carry their
// own tools, attachments, and fallback notices, and the fragment refreshes nothing
// out-of-band, so nothing else is read.
func (s *Store) SessionEarlierByID(ctx context.Context, sessionID int64, before int) (SessionDetail, TranscriptPage, error) {
	var d SessionDetail
	var page TranscriptPage
	err := s.snapshotTx(ctx, func(tx pgx.Tx) error {
		var err error
		if d, err = s.scanDetail(ctx, tx, "s.id = $1", sessionID); err != nil {
			return err
		}
		page, err = s.transcriptTail(ctx, tx, sessionID, &before)
		return err
	})
	if err != nil {
		return SessionDetail{}, TranscriptPage{}, err
	}
	return d, page, nil
}
