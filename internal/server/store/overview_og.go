package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// OverviewOGImage is a cached Open Graph preview card: the rendered PNG bytes and
// when they were generated. The generated_at stamp drives the TTL (a request past
// the cache window re-renders) and the cleanup sweep (an expired card is pruned).
type OverviewOGImage struct {
	PNG         []byte
	GeneratedAt time.Time
}

// PutOverviewOGImage stores the rendered preview card for a user's published
// overview, stamped with the instant the card's analytics were taken (generatedAt),
// not the write time. It upserts on the one-per-user key, but only overwrites a card
// that is not newer than this one: the DO UPDATE is guarded on
// EXCLUDED.generated_at >= the stored generated_at. So when concurrent requests race
// to regenerate an expired card, a render that read an older analytics snapshot but
// finishes last cannot clobber a newer card and make stale content look fresh for a
// whole TTL. Ties (equal timestamps) win harmlessly, since the render is
// deterministic for a given analytics window.
//
// It reports whether this card became the cached one: true when the row was inserted
// or the guarded update fired, false when a newer card was already present and the
// write was skipped. The caller uses that to avoid serving bytes it rendered but did
// not store (see ogimage.Generate), so the served image never diverges from the cache.
func (s *Store) PutOverviewOGImage(ctx context.Context, userID int64, png []byte, generatedAt time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx,
		`INSERT INTO overview_og_images (user_id, png, generated_at)
		      VALUES ($1, $2, $3)
		 ON CONFLICT (user_id)
		 DO UPDATE SET png = EXCLUDED.png, generated_at = EXCLUDED.generated_at
		       WHERE EXCLUDED.generated_at >= overview_og_images.generated_at`,
		userID, png, generatedAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// OverviewOGImage loads the cached preview card for a user, addressed by id, or
// ErrNotFound when none is cached yet. It is a plain by-id read with no visibility
// join: the public serve path reads through PublicOverviewCard, which folds in the
// overview_public gate atomically. This by-id form backs the render path's own
// reconciliation (Generate reloads the canonical card after a skipped guarded write)
// and the tests, where the visibility gate is not the property under test.
func (s *Store) OverviewOGImage(ctx context.Context, userID int64) (OverviewOGImage, error) {
	var img OverviewOGImage
	err := s.Pool.QueryRow(ctx,
		`SELECT png, generated_at FROM overview_og_images WHERE user_id = $1`,
		userID).Scan(&img.PNG, &img.GeneratedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OverviewOGImage{}, ErrNotFound
	}
	return img, err
}

// DeleteExpiredOGImages removes cached preview cards stamped before the cutoff, the
// housekeeping the cleanup loop runs. A card for a shared overview re-renders on
// demand, so pruning a stale one only discards bytes nobody is serving. It returns
// how many rows it removed.
func (s *Store) DeleteExpiredOGImages(ctx context.Context, olderThan time.Time) (int64, error) {
	tag, err := s.Pool.Exec(ctx,
		`DELETE FROM overview_og_images WHERE generated_at < $1`, olderThan)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
