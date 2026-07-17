package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ProjectPublication is the account settings read model for repository overview
// pages. It omits analytics rollups so loading account settings does not
// aggregate the session corpus just to populate a selector.
type ProjectPublication struct {
	ID             int64
	DisplayName    string
	RemoteKey      string
	OverviewPublic bool
}

// ListProjectPublications returns repository projects in stable display order.
// Standalone and orphaned folders are excluded because their machine-specific
// paths do not identify shareable repositories.
func (s *Store) ListProjectPublications(ctx context.Context) ([]ProjectPublication, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT id, display_name, remote_key, overview_public
		   FROM projects
		  WHERE kind = 'remote'
		  ORDER BY lower(COALESCE(NULLIF(display_name, ''), remote_key)), id`)
	if err != nil {
		return nil, fmt.Errorf("list project publications: %w", err)
	}
	defer rows.Close()
	var projects []ProjectPublication
	for rows.Next() {
		var project ProjectPublication
		if err := rows.Scan(&project.ID, &project.DisplayName, &project.RemoteKey, &project.OverviewPublic); err != nil {
			return nil, fmt.Errorf("scan project publication: %w", err)
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list project publications: %w", err)
	}
	return projects, nil
}

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

// PublishProjectOverview marks a project's usage overview public, so /p/<id>
// resolves for logged-out viewers. Projects are fleet-global rather than owned, so
// (unlike PublishSession) there is no owner check: any signed-in caller may flip the
// gate, matching the route's requireFull guard. The address is the project id, so
// there is no capability id to mint. A missing project touches no row and is
// ErrNotFound rather than a silent no-op.
func (s *Store) PublishProjectOverview(ctx context.Context, projectID int64) error {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE projects SET overview_public = TRUE WHERE id = $1`, projectID)
	if err != nil {
		return fmt.Errorf("publish project overview for project %d: %w", projectID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UnpublishProjectOverview hides a project's public overview by clearing the gate
// flag. The URL is the project id and never changes, so re-publishing later brings
// the same /p/<id> back.
func (s *Store) UnpublishProjectOverview(ctx context.Context, projectID int64) error {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE projects SET overview_public = FALSE WHERE id = $1`, projectID)
	if err != nil {
		return fmt.Errorf("unpublish project overview for project %d: %w", projectID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// PublicProjectOverview resolves a project id to its identity for the logged-out
// overview page, only while that project's overview is public. The flag is folded
// into the WHERE clause, so an unknown or unpublished id yields ErrNotFound and the
// link 404s. It returns the same identity fields Project does (no rollups), which the
// public page pairs with a windowed analytics read.
func (s *Store) PublicProjectOverview(ctx context.Context, id int64) (ProjectSummary, error) {
	var p ProjectSummary
	err := s.Pool.QueryRow(ctx,
		`SELECT id, remote_key, host, owner, repo, display_name, kind, overview_public
		   FROM projects
		  WHERE id = $1 AND overview_public = TRUE`, id).
		Scan(&p.ID, &p.RemoteKey, &p.Host, &p.Owner, &p.Repo, &p.DisplayName, &p.Kind, &p.OverviewPublic)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectSummary{}, ErrNotFound
	}
	if err != nil {
		return ProjectSummary{}, fmt.Errorf("read public project overview for project %d: %w", id, err)
	}
	return p, nil
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

// PublicOverviewCard resolves a username to its account and reads that account's
// cached Open Graph card in one query, gated on overview_public = TRUE. Folding the
// public check, the user lookup, and the card read into a single statement is what
// keeps the /u/<username>/og.png serve atomic: a split (resolve the user, then read
// the card) leaves a window where a concurrent unpublish between the two steps could
// serve a card for an overview that just went private. found is false when the name
// is unknown or the overview is not public, so the caller 404s the link. When found
// is true the account is public; card.PNG is nil when no card is cached yet (the
// LEFT JOIN yields NULLs), which the caller renders on demand. Only the fields the
// card path needs are loaded; the password hash is left zero.
func (s *Store) PublicOverviewCard(ctx context.Context, username string) (User, OGImage, bool, error) {
	var u User
	var png []byte
	var generatedAt *time.Time
	err := s.Pool.QueryRow(ctx,
		`SELECT u.id, u.username, u.overview_public, o.png, o.generated_at
		   FROM users u
		   LEFT JOIN overview_og_images o ON o.user_id = u.id
		  WHERE u.username = $1 AND u.overview_public = TRUE`,
		username).Scan(&u.ID, &u.Username, &u.OverviewPublic, &png, &generatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, OGImage{}, false, nil
	}
	if err != nil {
		return User{}, OGImage{}, false, fmt.Errorf("read public overview card for %q: %w", username, err)
	}
	var card OGImage
	if png != nil && generatedAt != nil {
		card = OGImage{PNG: png, GeneratedAt: *generatedAt}
	}
	return u, card, true, nil
}
