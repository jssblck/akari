package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PublicSessionSnapshot is the bounded state rendered by a public session page or
// one of its earlier-page requests. ProjectionRevision identifies the committed
// projection so a browser cannot append rows from two rebuilds. Outline and Tools are
// nil on an earlier-page request (before != nil): the outline rail already covers the
// whole session and does not change page to page, so only the first load and a
// stale-revision resync (which rerenders the whole body) need to pay for the read.
type PublicSessionSnapshot struct {
	Audit              SessionAudit
	Page               TranscriptPage
	Outline            []Message
	Tools              []ToolCallView
	ProjectionRevision int64
}

// PublicSessionByID loads a published session and one transcript window from a
// single repeatable-read snapshot. before is nil for the trailing window (also the
// whole-session outline and tool metadata the rail needs) and names the first
// currently rendered ordinal when paging backward (the outline read is skipped, since
// the fragment it feeds does not render one). Publication is checked inside the same
// transaction as every rendered row.
func (s *Store) PublicSessionByID(ctx context.Context, publicID string, before *int) (PublicSessionSnapshot, error) {
	var snap PublicSessionSnapshot
	err := s.snapshotTx(ctx, func(tx pgx.Tx) error {
		var err error
		if snap.Audit.Detail, err = s.scanDetail(ctx, tx,
			"s.public_id = $1 AND s.visibility = 'public'", publicID); err != nil {
			return err
		}
		sessionID := snap.Audit.Detail.ID
		if err := tx.QueryRow(ctx,
			`SELECT projection_revision FROM session_raw WHERE session_id = $1`,
			sessionID).Scan(&snap.ProjectionRevision); err != nil {
			return fmt.Errorf("read projection revision for public session %d: %w", sessionID, err)
		}
		if snap.Audit.Signals, err = s.sessionSignals(ctx, tx, sessionID); err != nil {
			return err
		}
		if snap.Audit.Subagents, err = s.subagents(ctx, tx, sessionID); err != nil {
			return err
		}
		if snap.Audit.Detail.ModelFallbackCount > 0 {
			if snap.Audit.Fallbacks, err = s.sessionModelFallbacks(ctx, tx, sessionID, ModelFallbackListCap); err != nil {
				return err
			}
		}
		if before == nil {
			if snap.Outline, snap.Tools, err = s.wholeSessionShape(ctx, tx, sessionID); err != nil {
				return err
			}
		}
		snap.Page, err = s.transcriptTail(ctx, tx, sessionID, before)
		return err
	})
	if err != nil {
		return PublicSessionSnapshot{}, err
	}
	return snap, nil
}
