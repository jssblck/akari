package main

import (
	"context"
	"log"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// runOGCleanup prunes expired Open Graph preview cards from the cache on a fixed
// interval until the context is cancelled. The cards (per-user overview, per-project
// overview, and per-session) are rendered lazily on request and cached for ttl (see
// the *.png handlers); a card for something nobody shares anymore would otherwise sit
// in its table past its usefulness, so each pass deletes anything older than ttl. It
// is pure housekeeping: a live share re-renders its card on demand regardless of this
// loop.
func runOGCleanup(ctx context.Context, st *store.Store, interval, ttl time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cleanupExpiredOGImages(ctx, st, ttl)
		}
	}
}

// cleanupExpiredOGImages deletes cached cards older than ttl in one pass, bounded by
// its own timeout so a slow delete cannot stack up behind the ticker. It sweeps all
// three card tables; a failure on one is logged but does not skip the others, so one
// table's transient error cannot let the others grow unbounded.
func cleanupExpiredOGImages(ctx context.Context, st *store.Store, ttl time.Duration) {
	passCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	cutoff := time.Now().Add(-ttl)
	sweeps := []struct {
		kind   string
		delete func(context.Context, time.Time) (int64, error)
	}{
		{"overview", st.DeleteExpiredOGImages},
		{"project", st.DeleteExpiredProjectOGImages},
		{"session", st.DeleteExpiredSessionOGImages},
	}
	var total int64
	for _, s := range sweeps {
		n, err := s.delete(passCtx, cutoff)
		if err != nil {
			log.Printf("og cleanup (%s): %v", s.kind, err)
			continue
		}
		total += n
	}
	if total > 0 {
		log.Printf("og cleanup: pruned %d expired card(s)", total)
	}
}
