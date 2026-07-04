package store

import (
	"context"
	"time"
)

// PutOverviewOGImage stores the rendered preview card for a user's published overview.
// The guarded-upsert semantics (and why the stamp is the data snapshot's instant, not
// the write time) live on putOGImage, shared by all three card caches.
func (s *Store) PutOverviewOGImage(ctx context.Context, userID int64, png []byte, generatedAt time.Time) (bool, error) {
	return s.putOGImage(ctx, overviewOGTable, userID, png, generatedAt)
}

// OverviewOGImage loads the cached preview card for a user, addressed by id, or
// ErrNotFound when none is cached yet. See ogImage for why this read carries no
// visibility join (the public serve path reads through PublicOverviewCard).
func (s *Store) OverviewOGImage(ctx context.Context, userID int64) (OGImage, error) {
	return s.ogImage(ctx, overviewOGTable, userID)
}

// DeleteExpiredOGImages removes cached overview cards stamped before the cutoff, the
// housekeeping the cleanup loop runs.
func (s *Store) DeleteExpiredOGImages(ctx context.Context, olderThan time.Time) (int64, error) {
	return s.deleteExpiredOGImages(ctx, overviewOGTable, olderThan)
}
