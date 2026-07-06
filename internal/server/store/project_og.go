package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// PutProjectOGImage stores the rendered preview card for a project's published
// overview. The guarded-upsert semantics live on putOGImage, shared by all three card
// caches.
func (s *Store) PutProjectOGImage(ctx context.Context, projectID int64, png []byte, generatedAt time.Time) (bool, error) {
	return s.putOGImage(ctx, projectOGTable, projectID, png, generatedAt)
}

// ProjectOGImage loads the cached preview card for a project, addressed by id, or
// ErrNotFound when none is cached yet. See ogImage for why this read carries no
// visibility join (the public serve path reads through PublicProjectCard).
func (s *Store) ProjectOGImage(ctx context.Context, projectID int64) (OGImage, error) {
	return s.ogImage(ctx, projectOGTable, projectID)
}

// PublicProjectCard resolves a project id to its identity and reads that project's
// cached Open Graph card in one query, gated on overview_public = TRUE. Folding the
// public check, the project lookup, and the card read into a single statement keeps
// the /p/<id>/og.png serve atomic: a split (resolve the project, then read the card)
// leaves a window where a concurrent unpublish between the two steps could serve a
// card for an overview that just went private. found is false when the id is unknown
// or the overview is not public, so the caller 404s the link. When found is true the
// project is public; card.PNG is nil when no card is cached yet (the LEFT JOIN yields
// NULLs), which the caller renders on demand. It returns the same identity fields
// PublicProjectOverview does, which the card render uses for the heading.
func (s *Store) PublicProjectCard(ctx context.Context, id int64) (ProjectSummary, OGImage, bool, error) {
	var p ProjectSummary
	var png []byte
	var generatedAt *time.Time
	err := s.Pool.QueryRow(ctx,
		`SELECT p.id, p.remote_key, p.host, p.owner, p.repo, p.display_name, p.kind, p.overview_public,
		        o.png, o.generated_at
		   FROM projects p
		   LEFT JOIN project_og_images o ON o.project_id = p.id
		  WHERE p.id = $1 AND p.overview_public = TRUE`,
		id).Scan(&p.ID, &p.RemoteKey, &p.Host, &p.Owner, &p.Repo, &p.DisplayName, &p.Kind, &p.OverviewPublic,
		&png, &generatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectSummary{}, OGImage{}, false, nil
	}
	if err != nil {
		return ProjectSummary{}, OGImage{}, false, fmt.Errorf("read public project card for project %d: %w", id, err)
	}
	var card OGImage
	if png != nil && generatedAt != nil {
		card = OGImage{PNG: png, GeneratedAt: *generatedAt}
	}
	return p, card, true, nil
}

// DeleteExpiredProjectOGImages removes cached project cards stamped before the cutoff,
// the housekeeping the cleanup loop runs beside DeleteExpiredOGImages.
func (s *Store) DeleteExpiredProjectOGImages(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.deleteExpiredOGImages(ctx, projectOGTable, olderThan)
}
