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

// PutOverviewOGImage stores (or replaces) the rendered preview card for a user's
// published overview, stamping it now. It upserts on the one-per-user key, so a
// refresh overwrites the previous card in place.
func (s *Store) PutOverviewOGImage(ctx context.Context, userID int64, png []byte) error {
	_, err := s.Pool.Exec(ctx,
		`INSERT INTO overview_og_images (user_id, png, generated_at)
		      VALUES ($1, $2, now())
		 ON CONFLICT (user_id)
		 DO UPDATE SET png = EXCLUDED.png, generated_at = EXCLUDED.generated_at`,
		userID, png)
	return err
}

// OverviewOGImage loads the cached preview card for a user, addressed by id, or
// ErrNotFound when none is cached yet. The caller resolves the user first (and with
// it the overview_public gate via PublicOverviewUser), so this is a plain by-id
// cache read with no visibility join of its own.
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
