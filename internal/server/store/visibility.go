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

// PublishOverview marks a user's own usage overview public, so /u/<username>
// resolves for logged-out viewers. The address is the username, so there is no
// capability id to mint: the call only flips the gate.
func (s *Store) PublishOverview(ctx context.Context, userID int64) error {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE users SET overview_public = TRUE WHERE id = $1`, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UnpublishOverview hides a user's public overview by clearing the gate flag. The
// URL is the username and never changes, so re-publishing later brings the same
// /u/<username> back.
func (s *Store) UnpublishOverview(ctx context.Context, userID int64) error {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE users SET overview_public = FALSE WHERE id = $1`, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// PublicOverviewUser resolves a username to its account for the logged-out
// overview page, only while that account's overview is public. The flag is folded
// into the WHERE clause, so an unknown or unpublished name yields ErrNotFound and
// the link 404s. Only the fields the public page needs are loaded; the password
// hash is left zero.
func (s *Store) PublicOverviewUser(ctx context.Context, username string) (User, error) {
	var u User
	err := s.Pool.QueryRow(ctx,
		`SELECT id, username, overview_public
		   FROM users
		  WHERE username = $1 AND overview_public = TRUE`,
		username).Scan(&u.ID, &u.Username, &u.OverviewPublic)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}
