package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// OverviewOGImage is a stored Open Graph preview card: the rendered PNG bytes and
// when they were generated. The generated_at stamp drives the daily refresh (an
// old or absent card is regenerated).
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

// PublicOverviewOGImage loads the stored preview card for a published overview,
// addressed by username. The overview_public gate is folded into the WHERE clause,
// so an unknown, unpublished, or not-yet-rendered account yields ErrNotFound and
// the card 404s, matching how /u/<username> itself resolves. A published account
// whose card has not been rendered yet (the render lost a race with the crawler)
// also 404s here until the next generation fills it.
func (s *Store) PublicOverviewOGImage(ctx context.Context, username string) (OverviewOGImage, error) {
	var img OverviewOGImage
	err := s.Pool.QueryRow(ctx,
		`SELECT o.png, o.generated_at
		   FROM overview_og_images o
		   JOIN users u ON u.id = o.user_id
		  WHERE u.username = $1 AND u.overview_public = TRUE`,
		username).Scan(&img.PNG, &img.GeneratedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OverviewOGImage{}, ErrNotFound
	}
	return img, err
}

// PublicOverviewsNeedingOGImage returns the accounts whose published overview has
// no preview card or a card older than the cutoff, so the background refresh can
// regenerate exactly those. It carries only the id and username the render needs;
// the rest of the User is left zero (the password hash is never loaded here).
func (s *Store) PublicOverviewsNeedingOGImage(ctx context.Context, olderThan time.Time) ([]User, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT u.id, u.username
		   FROM users u
		   LEFT JOIN overview_og_images o ON o.user_id = u.id
		  WHERE u.overview_public = TRUE
		    AND (o.user_id IS NULL OR o.generated_at < $1)
		  ORDER BY u.id`,
		olderThan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}
