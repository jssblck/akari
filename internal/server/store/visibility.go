package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// PublishSession marks a session public and returns its public id, minting a new
// one only if it had none. The owner check is folded into the WHERE clause, so a
// session that does not belong to the user returns ErrNotFound and is untouched.
// Re-publishing an already-public session keeps the existing id, so a shared link
// stays valid across repeated publishes.
func (s *Store) PublishSession(ctx context.Context, sessionID, userID int64, candidateID string) (string, error) {
	var publicID string
	err := s.Pool.QueryRow(ctx,
		`UPDATE sessions
		    SET visibility = 'public', public_id = COALESCE(public_id, $3)
		  WHERE id = $1 AND user_id = $2
		 RETURNING public_id`,
		sessionID, userID, candidateID).Scan(&publicID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return publicID, err
}

// DeleteSession removes a session and everything derived from it. The foreign
// keys cascade messages, tool calls, usage events, attachments, and the raw
// bytes; child sessions have their parent pointer nulled. Any CAS blobs the
// session referenced are left for a later SweepBlobs to reclaim. Authorization
// (owner or admin) is enforced by the caller, so this deletes unconditionally by
// id and returns ErrNotFound when nothing matched.
func (s *Store) DeleteSession(ctx context.Context, sessionID int64) error {
	tag, err := s.Pool.Exec(ctx, "DELETE FROM sessions WHERE id = $1", sessionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UnpublishSession returns a session to internal visibility and clears its public
// id, so the old link stops resolving rather than merely flipping a flag. It is
// owner-scoped; a session the user does not own yields ErrNotFound.
func (s *Store) UnpublishSession(ctx context.Context, sessionID, userID int64) error {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE sessions
		    SET visibility = 'internal', public_id = NULL
		  WHERE id = $1 AND user_id = $2`,
		sessionID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
