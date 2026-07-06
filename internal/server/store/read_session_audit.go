package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// The session detail's audit header reads: which models served the session, what the
// whole work item (the session plus its fan-out) cost, and how each direct subagent
// ended. All are read-time presentation reads over existing projection rows; none is
// rebuild-derived, so none moves parse.Epoch.

// SessionAudit is everything the audit header judges together, read from one MVCC
// snapshot: the session row, its current signals, its direct subagents with their
// verdicts, the whole-work-item rollup, and the serving models. The header shows the
// session's own cost, the subagents' costs, and the rollup of both side by side; a
// rebuild committing between separate reads could update sessions.total_cost_usd under
// the page and make those figures disagree, so they are pinned the same way the feed
// pins its rows (analyticsSnapshot's reasoning at single-session scale).
type SessionAudit struct {
	Detail    SessionDetail
	Signals   SessionSignals
	Subagents []SubagentRow
	Tree      TreeRollup
	Models    []string
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
	roll, err := s.treeRollups(ctx, tx, []int64{sessionID})
	if err != nil {
		return a, err
	}
	a.Tree = roll[sessionID]
	a.Models, err = s.sessionModels(ctx, tx, sessionID)
	return a, err
}

// SessionModels returns the distinct models that served a session, heaviest first by
// total token volume, capped so a pathological session cannot grow the header line. The
// header shows the working set, not an exhaustive ledger; the per-model split lives on
// the Insights instruments.
func (s *Store) SessionModels(ctx context.Context, sessionID int64) ([]string, error) {
	return s.sessionModels(ctx, s.Pool, sessionID)
}

func (s *Store) sessionModels(ctx context.Context, q querier, sessionID int64) ([]string, error) {
	rows, err := q.Query(ctx,
		`SELECT model FROM usage_events
		  WHERE session_id = $1 AND model <> ''
		  GROUP BY model
		  ORDER BY sum(coalesce(input_tokens,0) + coalesce(output_tokens,0) +
		               coalesce(cache_read_tokens,0) + coalesce(cache_write_tokens,0)) DESC,
		           model
		  LIMIT 6`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query models for session %d: %w", sessionID, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("scan model for session %d: %w", sessionID, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate models for session %d: %w", sessionID, err)
	}
	return out, nil
}

// TreeRollupFor computes one session's whole-work-item rollup (its own cost plus every
// subagent in its subtree), the same recursive fold the feed attaches per page
// (attachTreeRollups), for the session detail's audit header. A session that spawned
// nothing returns the zero rollup, which the header reads as "no fan-out".
func (s *Store) TreeRollupFor(ctx context.Context, sessionID int64) (TreeRollup, error) {
	roll, err := s.treeRollups(ctx, s.Pool, []int64{sessionID})
	if err != nil {
		return TreeRollup{}, err
	}
	return roll[sessionID], nil
}

// SubagentRow is one direct child session in a parent's subagents table: the summary
// every session list shows, plus the child's own verdict so the fold summary can say
// "2 failed" and the table can flag the children worth opening. Grade is nil and
// Outcome empty when the child has no current signals row (unsettled, or stale under a
// newer epoch), the same LEFT JOIN convention the feed row uses.
type SubagentRow struct {
	SessionSummary
	Grade   *string
	Outcome string
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
		       s.total_cost_usd, s.cost_incomplete, s.visibility, s.public_id,
		       s.started_at, s.ended_at, s.last_active_at,
		       sig.grade, sig.outcome,
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
			&r.TotalCostUSD, &r.CostIncomplete, &r.Visibility, &r.PublicID,
			&r.StartedAt, &r.EndedAt, &r.LastActiveAt,
			&r.Grade, &outcome,
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
