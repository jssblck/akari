package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The session detail's audit read covers how each direct subagent ended and the header
// tile's model-fallback list. Both are read-time presentation reads over existing
// projection rows; neither is rebuild-derived, so neither moves parse.Epoch.

// SessionAudit is everything the session instruments read together, from one MVCC
// snapshot: the session row, its current signals, and its direct subagents with their
// verdicts. The stat band shows the session's own cost beside the subagents' costs; a
// rebuild committing between separate reads could update sessions.total_cost_usd under
// the page and make those figures disagree, so they are pinned the same way the feed
// pins its rows (analyticsSnapshot's reasoning at single-session scale).
type SessionAudit struct {
	Detail    SessionDetail
	Signals   SessionSignals
	Subagents []SubagentRow
	// Fallbacks is the header tile's capped fallback list (ModelFallbackListCap rows),
	// loaded only when Detail's rollup counted one, from the same snapshot as the
	// count, so the tile's list and its "plus N more" remainder always reconcile.
	Fallbacks []ModelFallback
}

// SessionAuditByID loads a session's audit bundle from one repeatable-read snapshot.
// A missing session returns ErrNotFound.
func (s *Store) SessionAuditByID(ctx context.Context, sessionID int64) (SessionAudit, error) {
	var a SessionAudit
	err := s.snapshotTx(ctx, func(tx pgx.Tx) error {
		var err error
		a, err = s.sessionAudit(ctx, tx, sessionID)
		return err
	})
	if err != nil {
		return SessionAudit{}, err
	}
	return a, nil
}

// sessionAudit is SessionAuditByID inside a caller-owned transaction, so the session
// snapshot reads can pin the audit rows to the same MVCC snapshot as the transcript
// window and shape beside them.
func (s *Store) sessionAudit(ctx context.Context, tx pgx.Tx, sessionID int64) (SessionAudit, error) {
	var a SessionAudit
	var err error
	if a.Detail, err = s.scanDetail(ctx, tx, "s.id = $1", sessionID); err != nil {
		return a, err
	}
	if a.Signals, err = s.sessionSignals(ctx, tx, sessionID); err != nil {
		return a, err
	}
	if a.Subagents, err = s.subagents(ctx, tx, sessionID); err != nil {
		return a, err
	}
	// Only a session whose rollup counted a fallback pays for the list read; the
	// common no-fallback session skips it.
	if a.Detail.ModelFallbackCount > 0 {
		if a.Fallbacks, err = s.sessionModelFallbacks(ctx, tx, sessionID, ModelFallbackListCap); err != nil {
			return a, err
		}
	}
	return a, nil
}

// SubagentRow is one direct child session in a parent's subagents table: the summary
// every session list shows, plus the child's own verdict so the fold summary can say
// "2 failed" and the table can flag the children worth opening. Grade is nil and
// Outcome empty when the child has no current signals row (unsettled, or stale under a
// newer epoch), the same LEFT JOIN convention the feed row uses.
type SubagentRow struct {
	SessionSummary
	Grade        *string
	Outcome      string
	SubagentName string
}

// Failed reports whether this child ended in an error, the one outcome the fold summary
// counts as failed: an abandoned child is the parent stopping it, not the child failing.
func (r SubagentRow) Failed() bool { return r.Outcome == "errored" }

// Subagents returns the sessions whose parent is the given session, each with its
// verdict and its first-prompt title, so the parent's subagents table reads as what each
// child was asked to do and how it ended rather than a bare id list. The verdict comes
// from the same signalsCurrent-gated LEFT JOIN the feed row uses, so a child's outcome
// here matches its own session page.
func (s *Store) Subagents(ctx context.Context, parentID int64) ([]SubagentRow, error) {
	return s.subagents(ctx, s.Pool, parentID)
}

func (s *Store) subagents(ctx context.Context, q querier, parentID int64) ([]SubagentRow, error) {
	rows, err := q.Query(ctx, `
		SELECT s.id, s.agent, s.machine, s.git_branch, u.username,
		       s.message_count, s.user_message_count, s.model_fallback_count,
		       s.total_input_tokens, s.total_output_tokens,
		       s.total_cache_write_tokens, s.total_cache_read_tokens,
		       s.total_cost_usd, s.visibility, s.public_id,
		       s.started_at, s.ended_at, s.last_active_at,
		       sig.grade, sig.outcome, s.subagent_name,
		       coalesce(title.content, '')
		  FROM sessions s
		  JOIN users u ON u.id = s.user_id
		  LEFT JOIN session_signals sig ON sig.session_id = s.id AND `+signalsCurrent()+`
		  `+titleLateralSQL+`
		 WHERE s.parent_session_id = $1
		 ORDER BY s.id`, parentID)
	if err != nil {
		return nil, fmt.Errorf("query subagents of session %d: %w", parentID, err)
	}
	defer rows.Close()
	var out []SubagentRow
	for rows.Next() {
		var r SubagentRow
		var outcome *string
		if err := rows.Scan(&r.ID, &r.Agent, &r.Machine, &r.GitBranch, &r.Username,
			&r.MessageCount, &r.UserMessageCount, &r.ModelFallbackCount,
			&r.TotalInput, &r.TotalOutput, &r.TotalCacheWrite, &r.TotalCacheRead,
			&r.TotalCostUSD, &r.Visibility, &r.PublicID,
			&r.StartedAt, &r.EndedAt, &r.LastActiveAt,
			&r.Grade, &outcome, &r.SubagentName,
			&r.Title); err != nil {
			return nil, fmt.Errorf("scan subagent of session %d: %w", parentID, err)
		}
		if outcome != nil {
			r.Outcome = *outcome
		}
		r.Title = squashSpaces(r.Title)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate subagents of session %d: %w", parentID, err)
	}
	return out, nil
}
