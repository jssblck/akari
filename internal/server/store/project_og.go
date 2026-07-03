package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// OGImage is a cached Open Graph preview card: the rendered PNG bytes and when they
// were generated. The generated_at stamp drives the TTL (a request past the cache
// window re-renders) and the cleanup sweep (an expired card is pruned). It is the
// read shape for the project and session card tables; the per-user overview card
// predates it and keeps its own OverviewOGImage type.
type OGImage struct {
	PNG         []byte
	GeneratedAt time.Time
}

// PutProjectOGImage stores the rendered preview card for a project's published
// overview, stamped with the instant the card's analytics were taken (generatedAt),
// not the write time. It upserts on the one-per-project key with the same guard
// PutOverviewOGImage uses: the DO UPDATE fires only when EXCLUDED.generated_at >= the
// stored generated_at, so a render that read an older analytics snapshot but finishes
// last cannot clobber a newer card and make stale content look fresh for a whole TTL.
// Ties win harmlessly, since the render is deterministic for a given analytics window.
//
// It reports whether this card became the cached one: true when the row was inserted
// or the guarded update fired, false when a newer card was already present. The caller
// uses that to avoid serving bytes it rendered but did not store (see
// ogimage.GenerateProject), so the served image never diverges from the cache.
func (s *Store) PutProjectOGImage(ctx context.Context, projectID int64, png []byte, generatedAt time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx,
		`INSERT INTO project_og_images (project_id, png, generated_at)
		      VALUES ($1, $2, $3)
		 ON CONFLICT (project_id)
		 DO UPDATE SET png = EXCLUDED.png, generated_at = EXCLUDED.generated_at
		       WHERE EXCLUDED.generated_at >= project_og_images.generated_at`,
		projectID, png, generatedAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ProjectOGImage loads the cached preview card for a project, addressed by id, or
// ErrNotFound when none is cached yet. It is a plain by-id read with no visibility
// join: the public serve path reads through PublicProjectCard, which folds in the
// overview_public gate atomically. This by-id form backs the render path's own
// reconciliation (GenerateProject reloads the canonical card after a skipped guarded
// write) and the tests, where the visibility gate is not the property under test.
func (s *Store) ProjectOGImage(ctx context.Context, projectID int64) (OGImage, error) {
	var img OGImage
	err := s.Pool.QueryRow(ctx,
		`SELECT png, generated_at FROM project_og_images WHERE project_id = $1`,
		projectID).Scan(&img.PNG, &img.GeneratedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OGImage{}, ErrNotFound
	}
	return img, err
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
// the housekeeping the cleanup loop runs beside DeleteExpiredOGImages. A card for a
// shared overview re-renders on demand, so pruning a stale one only discards bytes
// nobody is serving. It returns how many rows it removed.
func (s *Store) DeleteExpiredProjectOGImages(ctx context.Context, olderThan time.Time) (int64, error) {
	tag, err := s.Pool.Exec(ctx,
		`DELETE FROM project_og_images WHERE generated_at < $1`, olderThan)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
