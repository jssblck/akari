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
// window re-renders) and the cleanup sweep (an expired card is pruned). It is the one
// read shape for all three card caches (overview, project, session).
type OGImage struct {
	PNG         []byte
	GeneratedAt time.Time
}

// ogTable names one card cache table and its one-per-subject key column. The three
// caches share one behavior, so the guarded upsert, the by-id read, and the TTL sweep
// are written once against this descriptor; the identifiers are package constants,
// never user input, so interpolating them into SQL is safe.
type ogTable struct{ table, keyCol string }

var (
	overviewOGTable = ogTable{"overview_og_images", "user_id"}
	projectOGTable  = ogTable{"project_og_images", "project_id"}
	sessionOGTable  = ogTable{"session_og_images", "session_id"}
)

// putOGImage stores a rendered preview card, stamped with the instant the card's data
// was read (generatedAt), not the write time. It upserts on the one-per-subject key,
// but only overwrites a card that is not newer than this one: the DO UPDATE is guarded
// on EXCLUDED.generated_at >= the stored generated_at. So when concurrent requests race
// to regenerate an expired card, a render that read an older data snapshot but finishes
// last cannot clobber a newer card and make stale content look fresh for a whole TTL.
// Ties (equal timestamps) win harmlessly, since the render is deterministic for a given
// snapshot.
//
// It reports whether this card became the cached one: true when the row was inserted or
// the guarded update fired, false when a newer card was already present and the write
// was skipped. The caller uses that to avoid serving bytes it rendered but did not
// store (see the ogimage generate paths), so the served image never diverges from the
// cache.
func (s *Store) putOGImage(ctx context.Context, t ogTable, key int64, png []byte, generatedAt time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx, fmt.Sprintf(
		`INSERT INTO %[1]s (%[2]s, png, generated_at)
		      VALUES ($1, $2, $3)
		 ON CONFLICT (%[2]s)
		 DO UPDATE SET png = EXCLUDED.png, generated_at = EXCLUDED.generated_at
		       WHERE EXCLUDED.generated_at >= %[1]s.generated_at`, t.table, t.keyCol),
		key, png, generatedAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// ogImage loads a cached preview card by its subject id, or ErrNotFound when none is
// cached yet. It is a plain by-id read with no visibility join: the public serve paths
// read through the Public*Card queries, which fold in their visibility gate atomically.
// This by-id form backs the render paths' own reconciliation (a generate reloads the
// canonical card after a skipped guarded write) and the tests, where the visibility
// gate is not the property under test.
func (s *Store) ogImage(ctx context.Context, t ogTable, key int64) (OGImage, error) {
	var img OGImage
	err := s.Pool.QueryRow(ctx, fmt.Sprintf(
		`SELECT png, generated_at FROM %s WHERE %s = $1`, t.table, t.keyCol),
		key).Scan(&img.PNG, &img.GeneratedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OGImage{}, ErrNotFound
	}
	return img, err
}

// deleteExpiredOGImages removes cached cards stamped before the cutoff, the
// housekeeping the cleanup loop runs per cache. A card re-renders on demand, so
// pruning a stale one only discards bytes nobody is serving. It returns how many rows
// it removed.
func (s *Store) deleteExpiredOGImages(ctx context.Context, t ogTable, olderThan time.Time) (int64, error) {
	tag, err := s.Pool.Exec(ctx, fmt.Sprintf(
		`DELETE FROM %s WHERE generated_at < $1`, t.table), olderThan)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
