package main

import (
	"context"
	"log"
	"time"

	"github.com/jssblck/akari/internal/server/store"
)

// runOGCleanup prunes expired Open Graph preview cards from the cache on a fixed
// interval until the context is cancelled. The cards are rendered lazily on request
// and cached for ttl (see the /u/<username>/og.png handler); a card for an overview
// nobody shares anymore would otherwise sit in the table past its usefulness, so
// each pass deletes anything older than ttl. It is pure housekeeping: a live share
// re-renders its card on demand regardless of this loop.
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
// its own timeout so a slow delete cannot stack up behind the ticker.
func cleanupExpiredOGImages(ctx context.Context, st *store.Store, ttl time.Duration) {
	passCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	n, err := st.DeleteExpiredOGImages(passCtx, time.Now().Add(-ttl))
	if err != nil {
		log.Printf("og cleanup: %v", err)
		return
	}
	if n > 0 {
		log.Printf("og cleanup: pruned %d expired card(s)", n)
	}
}
